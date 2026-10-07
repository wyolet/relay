package catalog

import (
	"errors"
	"testing"
)

func TestErrNotFoundOnlyForMissingRefs(t *testing.T) {
	ic, err := Index(&Catalog{Hosts: []Host{
		{Name: "a", Models: []Binding{{Name: "shared", MetadataName: "model-a"}}},
		{Name: "b", Models: []Binding{{Name: "shared", MetadataName: "model-b"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ic.Resolve("nope@a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve missing: err = %v, want ErrNotFound", err)
	}
	if _, err := ic.ResolveModelSlug("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveModelSlug missing: err = %v, want ErrNotFound", err)
	}
	if _, _, err := ic.Resolve("shared"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve ambiguous: err = %v, want a non-ErrNotFound error", err)
	}
	if _, err := ic.ResolveModelSlug("shared"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveModelSlug ambiguous: err = %v, want a non-ErrNotFound error", err)
	}
	if _, _, err := ic.Resolve("!!!"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve invalid: err = %v, want a non-ErrNotFound error", err)
	}
}
