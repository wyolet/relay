package seed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
	relayconfig "github.com/wyolet/relay/config"
)

const shippedYAML = `apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: per-ip
  owner:
    kind: system
spec:
  rules:
    - meter: requests
      amount: 60
      window: 1m
      strategy: sliding-window
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: ceiling
  owner:
    kind: system
spec:
  enabled: false
  rules: []
`

// memRateLimits is an in-memory rate limit table with a unique name, like the real one.
type memRateLimits struct {
	rows []*ratelimit.RateLimit
	// beforeUpsert runs ahead of each insert, standing in for another writer.
	beforeUpsert func(*memRateLimits)
}

func (m *memRateLimits) List(context.Context) ([]*ratelimit.RateLimit, error) {
	return append([]*ratelimit.RateLimit(nil), m.rows...), nil
}

func (m *memRateLimits) Upsert(_ context.Context, rl *ratelimit.RateLimit) error {
	if m.beforeUpsert != nil {
		m.beforeUpsert(m)
	}
	for _, r := range m.rows {
		if r.Meta.Name == rl.Meta.Name && r.Meta.ID != rl.Meta.ID {
			return errors.New("duplicate key value violates unique constraint")
		}
	}
	c := *rl
	m.rows = append(m.rows, &c)
	return nil
}

func (m *memRateLimits) byName(name string) *ratelimit.RateLimit {
	for _, r := range m.rows {
		if r.Meta.Name == name {
			return r
		}
	}
	return nil
}

func loadShipped(t *testing.T, dir string) *SystemRateLimits {
	t.Helper()
	defs, err := LoadSystemRateLimits([]byte(shippedYAML), dir)
	if err != nil {
		t.Fatalf("LoadSystemRateLimits: %v", err)
	}
	return defs
}

func TestShippedSystemRateLimitsParse(t *testing.T) {
	defs, err := LoadSystemRateLimits(relayconfig.SystemRateLimits, "")
	if err != nil {
		t.Fatalf("shipped definitions: %v", err)
	}
	var names []string
	for _, rl := range defs.Rows {
		names = append(names, rl.Meta.Name)
	}
	want := []string{"control-api", "inference-api", "inference-api-proxy", "inference-api-proxy-anonymous", "otlp-export"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestCreateMissingSystemRateLimitsCreatesEveryAbsentRow(t *testing.T) {
	store := &memRateLimits{}
	created, err := CreateMissingSystemRateLimits(context.Background(), store, loadShipped(t, "").Rows)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created, []string{"per-ip", "ceiling"}) {
		t.Fatalf("created = %v", created)
	}
	for _, r := range store.rows {
		if r.Meta.ID == "" || r.Meta.Owner.Kind != meta.OwnerSystem {
			t.Errorf("%s: id %q owner %q, want an id and a system owner", r.Meta.Name, r.Meta.ID, r.Meta.Owner.Kind)
		}
	}
}

func TestCreateMissingSystemRateLimitsLeavesExistingRowsAlone(t *testing.T) {
	off := false
	edited := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "per-ip", Owner: meta.Owner{Kind: meta.OwnerSystem}, Dirty: true},
		Spec: ratelimit.Spec{Enabled: &off, Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 5, Window: ratelimit.Window(time.Second), Strategy: ratelimit.StrategyFixedWindow,
		}}},
	}
	before := *edited
	store := &memRateLimits{rows: []*ratelimit.RateLimit{edited}}

	created, err := CreateMissingSystemRateLimits(context.Background(), store, loadShipped(t, "").Rows)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created, []string{"ceiling"}) {
		t.Fatalf("created = %v, want only the absent row", created)
	}
	if got := store.byName("per-ip"); !reflect.DeepEqual(*got, before) {
		t.Fatalf("existing row changed:\n got %+v\nwant %+v", *got, before)
	}

	again, err := CreateMissingSystemRateLimits(context.Background(), store, loadShipped(t, "").Rows)
	if err != nil || len(again) != 0 || len(store.rows) != 2 {
		t.Fatalf("second run: created %v err %v rows %d, want nothing new", again, err, len(store.rows))
	}
}

func TestCreateMissingSystemRateLimitsKeepsARowAnotherWriterCreated(t *testing.T) {
	store := &memRateLimits{}
	store.beforeUpsert = func(m *memRateLimits) {
		m.beforeUpsert = nil
		m.rows = append(m.rows, &ratelimit.RateLimit{Meta: meta.Metadata{ID: meta.NewID(), Name: "per-ip", Owner: meta.Owner{Kind: meta.OwnerSystem}}})
	}
	created, err := CreateMissingSystemRateLimits(context.Background(), store, loadShipped(t, "").Rows)
	if err != nil {
		t.Fatalf("a row created concurrently must not fail the boot: %v", err)
	}
	if !reflect.DeepEqual(created, []string{"ceiling"}) || len(store.rows) != 2 {
		t.Fatalf("created = %v rows = %d", created, len(store.rows))
	}
}

func TestLoadSystemRateLimitsTakesShippedNamesFromTheDirectory(t *testing.T) {
	dir := t.TempDir()
	override := `apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: per-ip
  owner:
    kind: system
spec:
  rules:
    - meter: requests
      amount: 5
      window: 1m
      strategy: sliding-window
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: not-shipped
  owner:
    kind: system
spec:
  rules:
    - meter: requests
      amount: 1
      window: 1m
      strategy: sliding-window
`
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	defs := loadShipped(t, dir)
	if len(defs.Rows) != 2 || defs.Rows[0].Spec.Rules[0].Amount != 5 {
		t.Fatalf("rows = %+v, want per-ip replaced by the directory's definition", defs.Rows)
	}
	if !reflect.DeepEqual(defs.FromDir, []string{"per-ip"}) || !reflect.DeepEqual(defs.Ignored, []string{"RateLimit/not-shipped"}) {
		t.Fatalf("fromDir = %v ignored = %v", defs.FromDir, defs.Ignored)
	}
}

func TestLoadSystemRateLimitsRefusesBadDefinitions(t *testing.T) {
	for name, yaml := range map[string]string{
		"enabled with no rules": `apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: per-ip
  owner:
    kind: system
spec:
  rules: []
`,
		"not system-owned": `apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: per-ip
  owner:
    kind: user
spec:
  enabled: false
  rules: []
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSystemRateLimits([]byte(yaml), ""); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
