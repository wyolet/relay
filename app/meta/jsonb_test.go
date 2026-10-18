package meta

import (
	"testing"
	"time"
)

func TestJSONB_DirtyRoundTrip(t *testing.T) {
	in := Metadata{ID: "id1", Name: "n", DisplayName: "N", Dirty: true}
	raw, err := MarshalJSONB(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalJSONB(in.ID, in.Name, in.DisplayName, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Dirty {
		t.Fatalf("dirty did not round-trip: %+v", out)
	}

	// Default (clean) stays clean.
	clean, _ := MarshalJSONB(Metadata{ID: "id2", Name: "c"})
	got, _ := UnmarshalJSONB("id2", "c", "", clean)
	if got.Dirty {
		t.Fatalf("clean row should not be dirty: %+v", got)
	}
}

func TestJSONB_CatalogDetachedAtRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	in := Metadata{ID: "id1", Name: "n", CatalogDetachedAt: &at}
	raw, err := MarshalJSONB(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalJSONB(in.ID, in.Name, in.DisplayName, raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.CatalogDetachedAt == nil || !out.CatalogDetachedAt.Equal(at) {
		t.Fatalf("catalogDetachedAt did not round-trip: %+v", out)
	}

	attached, _ := MarshalJSONB(Metadata{ID: "id2", Name: "c"})
	if got, _ := UnmarshalJSONB("id2", "c", "", attached); got.CatalogDetachedAt != nil {
		t.Fatalf("an attached row came back detached: %+v", got)
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
}

func TestJSONB_AnnotationsRoundTrip(t *testing.T) {
	in := Metadata{
		ID: "id1", Name: "n",
		Annotations: map[string]string{"wyolet.com/owner-email": "platform@example.com"},
	}
	raw, err := MarshalJSONB(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalJSONB(in.ID, in.Name, in.DisplayName, raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Annotations["wyolet.com/owner-email"] != "platform@example.com" {
		t.Fatalf("annotations did not round-trip: %+v", out.Annotations)
	}
}
