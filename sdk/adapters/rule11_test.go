package adapters_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// allowedDrop is one way a shape may lose a canonical field on
// canonical → wire → canonical. Exactly one of annotation, folded or carriable
// is set:
//   - annotation: an irreducible drop; the adapter sources must hold this text
//     in a `// canonical: <field> dropped — <why>` comment (rule 11).
//   - folded: not a drop — the value survives in another canonical field (a
//     hoisted system item merged into instructions, a tier the wire rounds to).
//   - carriable: the wire has a home for the field but the adapter does not
//     carry it yet; reported as a skipped subtest until fixed.
type allowedDrop struct {
	paths      []string
	annotation string
	folded     string
	carriable  string
}

func TestRule11FullRequestRoundTrip(t *testing.T) {
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			req := fullRequest(s.name)
			wire, err := s.tr.SerializeRequest(req)
			if err != nil {
				t.Fatalf("serialize full request: %v", err)
			}
			back, err := s.tr.ParseRequest(wire)
			if err != nil {
				t.Fatalf("parse serialized request: %v\nwire: %s", err, wire)
			}
			before, err := flattenRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			after, err := flattenRequest(back)
			if err != nil {
				t.Fatal(err)
			}
			checkDrops(t, s, lostPaths(before, after), before, after, wire)
		})
	}
}

func checkDrops(t *testing.T, s shape, lost []string, before, after map[string]string, wire []byte) {
	t.Helper()
	allow := allowedDrops[s.name]
	used := make([]bool, len(allow))
	var unexplained []string
	for _, p := range lost {
		i := matchDrop(allow, p)
		if i < 0 {
			unexplained = append(unexplained, p+"\n      sent: "+before[p]+"\n      back: "+after[p])
			continue
		}
		used[i] = true
	}
	if len(unexplained) > 0 {
		t.Errorf("fields lost with no allowlist entry (annotate the drop site, or carry the field):\n  %s\nwire: %s",
			strings.Join(unexplained, "\n  "), wire)
	}
	comments := canonicalComments(t, s.dir)
	for i, d := range allow {
		if !used[i] {
			t.Errorf("allowlist entry %v matched nothing — the field round-trips now; delete the entry", d.paths)
			continue
		}
		switch {
		case d.annotation != "":
			if !containsAny(comments, d.annotation) {
				t.Errorf("drop %v names annotation %q, which no `// canonical:` comment in %s/ holds", d.paths, d.annotation, s.dir)
			}
		case d.carriable != "":
			t.Run("carriable "+d.paths[0], func(t *testing.T) {
				t.Skip("field has a wire home but is dropped: " + d.carriable)
			})
		case d.folded == "":
			t.Errorf("allowlist entry %v gives no reason", d.paths)
		}
	}
}

func matchDrop(allow []allowedDrop, path string) int {
	for i, d := range allow {
		for _, pat := range d.paths {
			if globMatch(pat, path) {
				return i
			}
		}
	}
	return -1
}

// canonicalComments returns every non-test source comment line in the adapter
// package dir that carries a rule-11 annotation.
func canonicalComments(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "//"); i >= 0 && strings.Contains(line[i:], "canonical: ") {
				out = append(out, line[i:])
			}
		}
	}
	return out
}

func containsAny(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// TestRule11RejectedFeatures checks the inputs fullRequest leaves out: each
// shape either refuses them with an error or carries them through.
func TestRule11RejectedFeatures(t *testing.T) {
	for _, s := range shapes {
		for name, apply := range rejectedFeatures {
			t.Run(s.name+"/"+name, func(t *testing.T) {
				if bug := silentFeatureDrops[s.name][name]; bug != "" {
					t.Skip("dropped without an error: " + bug)
				}
				req := minimalRequest()
				apply(req)
				wire, err := s.tr.SerializeRequest(req)
				if err != nil {
					return // refused loudly
				}
				back, err := s.tr.ParseRequest(wire)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				assertLossless(t, req, back, wire)
			})
		}
	}
}

func assertLossless(t *testing.T, req, back *v1.Request, wire []byte) {
	t.Helper()
	before, err := flattenRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	after, err := flattenRequest(back)
	if err != nil {
		t.Fatal(err)
	}
	if lost := lostPaths(before, after); len(lost) > 0 {
		t.Errorf("serialized without an error but lost %v\nwire: %s", lost, wire)
	}
}
