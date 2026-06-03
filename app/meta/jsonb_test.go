package meta

import "testing"

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
