package modeldir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ChangeKind is the outcome of refreshing one file.
type ChangeKind string

const (
	// Updated: the catalog's entry differed and the file was rewritten.
	Updated ChangeKind = "updated"
	// Unchanged: the catalog's entry matches the file; the file was not written.
	Unchanged ChangeKind = "unchanged"
	// Missing: the catalog no longer serves the model at the file's pricedBy host; the file was kept.
	Missing ChangeKind = "missing"
)

// Change reports what Refresh did to one file.
type Change struct {
	Name string
	Kind ChangeKind
	// Fields lists the file keys whose value changed, in file order; set for Updated.
	Fields []string
	Before Model
	// After is the rewritten file; set for Updated.
	After Model
}

// Refresh re-derives every file in dir that carries a source from src, keeping its pricedBy host, and returns one Change per such file in name order. Hand-written files (no source) are left untouched and not reported; no file is ever deleted. src is only fetched when dir holds a derived file.
func Refresh(ctx context.Context, dir string, src Source) ([]Change, error) {
	models, err := List(dir)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, m := range models {
		if m.Source != nil {
			changes = append(changes, Change{Name: m.Name, Before: m})
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}
	ic, err := src.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("modeldir: source: %w", err)
	}
	for i := range changes {
		c := &changes[i]
		derived, err := deriveModel(ic, c.Name, c.Before.PricedBy)
		if errors.Is(err, errNotInCatalog) {
			c.Kind = Missing
			continue
		}
		if err != nil {
			return nil, err
		}
		fields, err := changedFields(c.Before, derived)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			c.Kind = Unchanged
			continue
		}
		if err := writeModelFile(dir, derived); err != nil {
			return nil, err
		}
		c.Kind, c.Fields, c.After = Updated, fields, derived
	}
	return changes, nil
}

// changedFields compares the encoded value of every key except source, so a file differing only in which catalog version it came from counts as unchanged and stays untouched.
func changedFields(before, after Model) ([]string, error) {
	bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
	var fields []string
	for i := range bv.NumField() {
		key, _, _ := strings.Cut(bv.Type().Field(i).Tag.Get("yaml"), ",")
		if key == "source" {
			continue
		}
		b, err := yaml.Marshal(bv.Field(i).Interface())
		if err != nil {
			return nil, fmt.Errorf("modeldir: compare %s: %w", before.Name, err)
		}
		a, err := yaml.Marshal(av.Field(i).Interface())
		if err != nil {
			return nil, fmt.Errorf("modeldir: compare %s: %w", before.Name, err)
		}
		if !bytes.Equal(b, a) {
			fields = append(fields, key)
		}
	}
	return fields, nil
}
