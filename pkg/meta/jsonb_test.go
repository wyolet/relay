package meta

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// The blob holds what has no column of its own; id, name, display name and the version come from columns and timestamps are not stored in it.
func TestJSONBRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	full := Metadata{
		ID: "id1", Name: "n", DisplayName: "N", Description: "d",
		Owner:  Owner{Kind: OwnerTeam, ID: "t-1"},
		Labels: map[string]string{"team": "infra"}, Annotations: map[string]string{"wyolet.com/owner-email": "platform@example.com"},
		Dirty: true, CatalogDetachedAt: &at,
		ResourceVersion: "7", CreatedAt: at, UpdatedAt: at,
	}
	for _, tc := range []struct {
		name string
		in   Metadata
		blob string
	}{
		{"every field", full, `{"description":"d","owner":{"kind":"team","id":"t-1"},"labels":{"team":"infra"},"annotations":{"wyolet.com/owner-email":"platform@example.com"},"dirty":true,"catalogDetachedAt":"2026-10-18T12:00:00Z"}`},
		{"identity only", Metadata{ID: "id2", Name: "c"}, `{"owner":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := MarshalJSONB(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.blob {
				t.Errorf("blob = %s\nwant   %s", raw, tc.blob)
			}
			got, err := UnmarshalJSONB(tc.in.ID, tc.in.Name, tc.in.DisplayName, raw)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.in
			want.ResourceVersion, want.CreatedAt, want.UpdatedAt = "", time.Time{}, time.Time{}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("metadata read back (-written +read):\n%s", diff)
			}
		})
	}

	// A row with no blob is its columns; a blob that is not JSON fails, still naming the row.
	got, err := UnmarshalJSONB("id", "name", "Display", nil)
	if diff := cmp.Diff(Metadata{ID: "id", Name: "name", DisplayName: "Display"}, got); diff != "" || err != nil {
		t.Errorf("empty blob: %v (-want +got):\n%s", err, diff)
	}
	got, err = UnmarshalJSONB("id", "name", "", []byte("{not json"))
	if err == nil || !strings.HasPrefix(err.Error(), "metadata: ") || got.ID != "id" || got.Name != "name" {
		t.Errorf("malformed blob = %+v, %v; want a metadata error beside the row's identity", got, err)
	}
}

func TestGovernedOwnerKind(t *testing.T) {
	at := time.Now()
	m := Metadata{Owner: Owner{Kind: OwnerHost, ID: "h1"}}
	if got := m.GovernedOwnerKind(); got != OwnerHost {
		t.Fatalf("attached row governed as %q, want its owner kind", got)
	}
	m.CatalogDetachedAt = &at
	if got := m.GovernedOwnerKind(); got != OwnerUser {
		t.Fatalf("detached row governed as %q, want user", got)
	}
	m.Owner = Owner{Kind: OwnerOrganization}
	if got := m.GovernedOwnerKind(); got != OwnerOrganization {
		t.Fatalf("detached organization row governed as %q, want organization", got)
	}
}
