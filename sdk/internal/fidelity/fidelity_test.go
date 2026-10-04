package fidelity

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// walkLeaves visits every leaf field of the canonical request reachable from
// v. Input is skipped (out of scope, see package doc); Tools.Definitions and
// ModelConfig descend into their single element (the fixture's FunctionTool
// and model entry).
func walkLeaves(v reflect.Value, prefix string, visit func(path string, leaf reflect.Value)) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() || (prefix == "" && sf.Name == "Input") {
			continue
		}
		path := sf.Name
		if prefix != "" {
			path = prefix + "." + sf.Name
		}
		fv := v.Field(i)
		ft := sf.Type
		switch {
		case ft == reflect.TypeOf(v1.Tools(nil)):
			if fv.Len() == 0 {
				visit(path, fv)
				continue
			}
			walkLeaves(fv.Index(0), path, visit)
		case ft.Kind() == reflect.Map && ft.Elem() == reflect.TypeOf(&v1.ModelOpts{}):
			if fv.Len() == 0 {
				visit(path, fv)
				continue
			}
			walkLeaves(fv.MapIndex(fv.MapKeys()[0]), path, visit)
		case ft != rawMessageType && (ft.Kind() == reflect.Struct || (ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct)):
			if ft.Kind() == reflect.Pointer && fv.IsNil() {
				visit(path, fv)
				continue
			}
			walkLeaves(fv, path, visit)
		default:
			visit(path, fv)
		}
	}
}

// recorder captures Check's failures instead of failing the real test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Helper()                        {}
func (r *recorder) Errorf(format string, a ...any) { r.errs = append(r.errs, format) }
func (r *recorder) Logf(string, ...any)            {}
func (r *recorder) Failed() bool                   { return len(r.errs) > 0 }

func TestCheckFailsOnSilentDropAndStaleAllowlist(t *testing.T) {
	r := &recorder{TB: t}
	Check(r, []byte(`{}`), nil, nil)
	if len(r.errs) != len(fields) {
		t.Errorf("empty body with no drop list: %d failures, want one per field (%d)", len(r.errs), len(fields))
	}

	r = &recorder{TB: t}
	Check(r, []byte(`{"user":"fidelity-user"}`), nil, map[string]string{"User": "stale"})
	found := false
	for _, e := range r.errs {
		if e == "%s: listed as dropped but %q is on the wire" {
			found = true
		}
	}
	if !found {
		t.Error("a dropped field still on the wire must fail")
	}
}

// A canonical field added without a fixture entry (or a fixture entry left
// behind by a removed field) fails here, before any adapter test runs.
func TestFieldsCoverCanonicalRequest(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range fields {
		listed[f.path] = true
	}
	walked := map[string]bool{}
	walkLeaves(reflect.ValueOf(Request()), "", func(path string, leaf reflect.Value) {
		walked[path] = true
		if !listed[path] {
			t.Errorf("canonical field %s has no fixture entry — add it to fields and Request()", path)
		}
		if leaf.IsZero() {
			t.Errorf("canonical field %s is unset in Request() — the check would pass vacuously", path)
		}
	})
	var stale []string
	for p := range listed {
		if !walked[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("fixture lists %s, which is no longer a canonical field", p)
	}
}
