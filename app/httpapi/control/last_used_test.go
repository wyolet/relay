package control

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
)

// fakeLastUsed answers from fixed maps and records every read.
type fakeLastUsed struct {
	keys, hostKeys         map[string]time.Time
	keyReads, hostKeyReads [][]string
	err                    error
}

func (f *fakeLastUsed) KeysLastUsed(_ context.Context, ids []string) (map[string]time.Time, error) {
	f.keyReads = append(f.keyReads, ids)
	return f.keys, f.err
}

func (f *fakeLastUsed) HostKeysLastUsed(_ context.Context, ids []string) (map[string]time.Time, error) {
	f.hostKeyReads = append(f.hostKeyReads, ids)
	return f.hostKeys, f.err
}

func lastUsedOf(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func TestKeyReadsCarryLastUsedFromOneRead(t *testing.T) {
	used := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	f := &fakeLastUsed{keys: map[string]time.Time{"k-1": used}, hostKeys: map[string]time.Time{"hk-2": used}}
	d := Deps{LastUsed: f, Authz: authz.AlwaysAllowAuthenticated{}}
	ctx := context.Background()

	keys := []*key.Key{{Meta: meta.Metadata{ID: "k-1"}}, {Meta: meta.Metadata{ID: "k-2"}}}
	enrichKeysRead(d)(ctx, keys)
	if diff := cmp.Diff([]string{"2026-11-15T10:30:00Z", ""}, []string{lastUsedOf(keys[0].LastUsedAt), lastUsedOf(keys[1].LastUsedAt)}); diff != "" {
		t.Errorf("key lastUsedAt (-want +got):\n%s", diff)
	}
	hks := []*hostkey.HostKey{{Meta: meta.Metadata{ID: "hk-1"}}, {Meta: meta.Metadata{ID: "hk-2"}}}
	enrichHostKeysRead(d)(ctx, hks)
	if diff := cmp.Diff([]string{"", "2026-11-15T10:30:00Z"}, []string{lastUsedOf(hks[0].LastUsedAt), lastUsedOf(hks[1].LastUsedAt)}); diff != "" {
		t.Errorf("host key lastUsedAt (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([][]string{{"k-1", "k-2"}}, f.keyReads); diff != "" {
		t.Errorf("key reads for a list (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([][]string{{"hk-1", "hk-2"}}, f.hostKeyReads); diff != "" {
		t.Errorf("host key reads for a list (-want +got):\n%s", diff)
	}

	one := &key.Key{Meta: meta.Metadata{ID: "k-1"}}
	enrichKeyRead(d)(ctx, one)
	hk := &hostkey.HostKey{Meta: meta.Metadata{ID: "hk-2"}}
	enrichHostKeyRead(d)(ctx, hk)
	if one.LastUsedAt == nil || hk.LastUsedAt == nil {
		t.Errorf("single reads: key %v, host key %v; want both set", one.LastUsedAt, hk.LastUsedAt)
	}
}

func TestKeyReadsLeaveLastUsedOutWhenTheReadFails(t *testing.T) {
	d := Deps{LastUsed: &fakeLastUsed{err: errors.New("pg down")}, Authz: authz.AlwaysAllowAuthenticated{}}
	k := &key.Key{Meta: meta.Metadata{ID: "k-1"}}
	enrichKeyRead(d)(context.Background(), k)
	hk := &hostkey.HostKey{Meta: meta.Metadata{ID: "hk-1"}}
	enrichHostKeyRead(d)(context.Background(), hk)
	if k.LastUsedAt != nil || hk.LastUsedAt != nil {
		t.Errorf("lastUsedAt = %v, %v; want both absent", k.LastUsedAt, hk.LastUsedAt)
	}
}

// lastUsedAt is server-derived: read-only, never required.
func TestControlOpenAPILastUsedReadOnly(t *testing.T) {
	doc := controlDoc(t)
	for _, schema := range []string{"Key", "HostKey"} {
		p, _ := schemaProps(t, doc, schema)["lastUsedAt"].(map[string]any)
		if p == nil || p["readOnly"] != true || p["format"] != "date-time" {
			t.Errorf("%s.lastUsedAt = %v, want a read-only date-time", schema, p)
		}
		comps, _ := doc["components"].(map[string]any)
		s, _ := comps["schemas"].(map[string]any)[schema].(map[string]any)
		req, _ := s["required"].([]any)
		if slices.Contains(req, any("lastUsedAt")) {
			t.Errorf("%s requires lastUsedAt", schema)
		}
	}
}
