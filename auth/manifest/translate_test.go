package manifest_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/manifest"
)

// A translator handed a document of another kind refuses it instead of building a row from an empty spec.
func TestTranslateRefusesADocumentOfAnotherKind(t *testing.T) {
	r := testRegistry(t)
	docs, err := r.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: platform}\n---\n" +
		"apiVersion: auth.wyolet.dev/v1alpha1\nkind: Role\nmetadata: {name: reader}\nspec: {rules: [{kinds: [models], verbs: [get]}]}\n"))
	must(t, err)
	team, role := &docs[0], &docs[1]

	for kind, translate := range map[string]func(*manifest.Document) (any, error){
		manifest.KindTeam:    func(d *manifest.Document) (any, error) { row, err := r.ToTeam(d, nil); return row == nil, err },
		manifest.KindProject: func(d *manifest.Document) (any, error) { row, err := r.ToProject(d, nil); return row == nil, err },
		manifest.KindServiceAccount: func(d *manifest.Document) (any, error) {
			row, err := r.ToServiceAccount(d, nil)
			return row == nil, err
		},
		manifest.KindRole:        func(d *manifest.Document) (any, error) { row, err := r.ToRole(d, nil); return row == nil, err },
		manifest.KindGroup:       func(d *manifest.Document) (any, error) { row, err := r.ToGroup(d, nil); return row == nil, err },
		manifest.KindRoleBinding: func(d *manifest.Document) (any, error) { row, err := r.ToRoleBinding(d, nil); return row == nil, err },
	} {
		d, want := team, `doc 0 Team "platform": kind: want a `+kind+` document`
		if kind == manifest.KindTeam {
			d, want = role, `doc 1 Role "reader": kind: want a Team document`
		}
		noRow, err := translate(d)
		if err == nil || err.Error() != want || noRow != true {
			t.Errorf("To%s of a %s document: no row %v, err %v; want %q", kind, d.Kind, noRow, err, want)
		}
		if e := asError(t, err); e.Field != "kind" {
			t.Errorf("To%s: error field %q, want kind", kind, e.Field)
		}
	}
}
