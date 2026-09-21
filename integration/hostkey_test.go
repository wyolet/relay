//go:build integration

// hostkey_test.go pins what a host key whose secret cannot be read does to
// the control plane. The snapshot drops it so no request picks a valueless
// credential, but the row itself has to stay readable, exportable and
// appliable — an operator repairing the reference is the only way out, and
// an endpoint that 500s on the broken row takes that away.
package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

// envRefEnvVar is deliberately never set while the row is being read.
const envRefEnvVar = "RELAY_INTEGRATION_UNSET_HOSTKEY"

// newDiscardUpstream stands in for a provider these tests never call: the
// happy-path seed needs a host base URL, not a working one.
func newDiscardUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type hostKeyRow struct {
	Metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Unresolved *struct {
			Reason string `json:"reason"`
		} `json:"unresolved"`
	} `json:"status"`
}

// waitForReload blocks until the catalog publishes a snapshot newer than
// gen. Catalog writes reach the snapshot over PG NOTIFY, which is the path
// a live deployment uses; forcing a rebuild in-process would skip it.
func (s *stack) waitForReload(t *testing.T, gen uint64) *appcatalog.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap := s.cat.Current(); snap.Generation() != gen {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog did not rebuild after the write")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForHostKey blocks until a rebuild lands that carries the key. Waiting
// on the generation alone is not enough: a debounced rebuild for an earlier
// write can bump it first.
func (s *stack) waitForHostKey(t *testing.T, id string) *hostkey.HostKey {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, ok := s.cat.Current().HostKey(id); ok {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("the repaired host key never reached the snapshot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// seedUnresolvableHostKey adds an env-ref host key pointing at a variable
// that is not set, alongside the happy path's working one.
func (s *stack) seedUnresolvableHostKey(t *testing.T) *hostkey.HostKey {
	t.Helper()
	ctx := context.Background()
	snap := s.cat.Current()
	hst, ok := snap.HostByName("test-host")
	if !ok {
		t.Fatal("the happy path host is missing")
	}
	tier, ok := snap.PolicyByName("test-host-tier")
	if !ok {
		t.Fatal("the host tier policy is missing")
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "broken-hostkey", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: hostkey.Spec{
			HostID:    hst.Meta.ID,
			PolicyID:  tier.Meta.ID,
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: envRefEnvVar},
		},
	}
	gen := s.cat.Current().Generation()
	if err := s.stores.HostKey.Upsert(ctx, hk); err != nil {
		t.Fatalf("seed host key: %v", err)
	}
	s.waitForReload(t, gen)
	return hk
}

func TestUnresolvableHostKeyStaysReadableAndOutOfTheSnapshot(t *testing.T) {
	st := newStack(t)
	st.seedHappyPath(newDiscardUpstream(t), "sk-mock-upstream-key")
	id := st.seedUnresolvableHostKey(t).Meta.ID

	// It lists, and says why it is broken rather than pretending it works.
	code, raw := st.adminDo(http.MethodGet, "/api/host-keys/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET host key = %d: %s", code, raw)
	}
	var row hostKeyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode host key: %v", err)
	}
	if row.Status.Unresolved == nil {
		t.Fatalf("the row reports no unresolved status: %s", raw)
	}
	if !strings.Contains(row.Status.Unresolved.Reason, envRefEnvVar) {
		t.Errorf("unresolved reason = %q, want it to name the missing variable", row.Status.Unresolved.Reason)
	}

	if code, raw := st.adminDo(http.MethodGet, "/api/host-keys", ""); code != http.StatusOK {
		t.Fatalf("LIST host keys = %d: %s", code, raw)
	}

	// The snapshot drops it, so no request can pick a valueless credential.
	if _, ok := st.cat.Current().HostKey(id); ok {
		t.Error("the unresolvable host key reached the snapshot")
	}

	// Export and re-apply both survive it: the repair path runs through them.
	code, raw = st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, raw)
	}
	bundle := string(raw)
	if !strings.Contains(bundle, "broken-hostkey") {
		t.Errorf("export omits the broken host key:\n%s", bundle)
	}
	if code, _, raw := st.applyBundle(bundle, ""); code != http.StatusOK {
		t.Fatalf("apply of the exported bundle = %d: %s", code, raw)
	}
}

// Once the variable is set the key resolves, and the next rebuild picks it
// up — the operator does not have to recreate the row.
func TestResolvingAHostKeyEnvRefPutsItBackInTheSnapshot(t *testing.T) {
	st := newStack(t)
	st.seedHappyPath(newDiscardUpstream(t), "sk-mock-upstream-key")
	hk := st.seedUnresolvableHostKey(t)
	id := hk.Meta.ID
	if _, ok := st.cat.Current().HostKey(id); ok {
		t.Fatal("the unresolvable host key reached the snapshot")
	}

	// Setting the variable changes nothing on its own — the value is read at
	// snapshot build, so the operator's next write to the row is what picks
	// it up.
	t.Setenv(envRefEnvVar, "sk-now-resolvable")
	if err := st.stores.HostKey.Upsert(context.Background(), hk); err != nil {
		t.Fatalf("touch host key: %v", err)
	}

	got := st.waitForHostKey(t, id)
	if got.Resolved != "sk-now-resolvable" {
		t.Errorf("resolved value = %q, want the variable's contents", got.Resolved)
	}
	code, raw := st.adminDo(http.MethodGet, "/api/host-keys/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET host key = %d: %s", code, raw)
	}
	var row hostKeyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode host key: %v", err)
	}
	if row.Status.Unresolved != nil {
		t.Errorf("the repaired row still reports unresolved: %s", raw)
	}
}
