package manifest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/manifest"
)

func parseErr(t *testing.T, src string) *manifest.Error {
	t.Helper()
	_, err := testRegistry(t).Parse(strings.NewReader(src))
	if err == nil {
		t.Fatalf("parse succeeded, want an error:\n%s", src)
	}
	return asError(t, err)
}

// A typo in a rule matches nothing at authorization time, so it must fail here, naming the document and the term.
func TestParseRefusesARuleTermOutsideTheVocabulary(t *testing.T) {
	e := parseErr(t, `apiVersion: auth.wyolet.dev/v1alpha1
kind: Team
metadata: {name: platform}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: Role
metadata: {name: editor}
spec:
  rules:
    - {kinds: [models], verbs: [get]}
    - {kinds: [models], verbs: [list, updat]}
`)
	if e.Doc != 1 || e.Kind != "Role" || e.Name != "editor" || e.Field != "spec.rules[1].verbs[1]" {
		t.Errorf("error at doc %d %s %q %s, want doc 1 Role \"editor\" spec.rules[1].verbs[1]", e.Doc, e.Kind, e.Name, e.Field)
	}
	if !strings.Contains(e.Error(), `"updat" is not a registered verb`) {
		t.Errorf("message = %q", e.Error())
	}
	k := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Role\nmetadata: {name: r}\nspec: {rules: [{kinds: [modles], verbs: [get]}]}\n")
	if k.Field != "spec.rules[0].kinds[0]" {
		t.Errorf("kind typo reported at %q", k.Field)
	}
}

func TestParseRefusesUnknownFields(t *testing.T) {
	for _, tc := range []struct{ name, src, field string }{
		{"spec", "kind: Group\nmetadata: {name: eng}\nspec: {membrs: [alice]}", "spec.membrs"},
		{"nested", "kind: RoleBinding\nmetadata: {name: b}\nspec: {role: r, scope: {kind: system, nmae: x}, subjects: [{kind: group, name: g}]}", "spec.scope.nmae"},
		{"list item", "kind: Role\nmetadata: {name: r}\nspec: {rules: [{kinds: [models], verbs: [get], scope: x}]}", "spec.rules[0].scope"},
		{"metadata", "kind: Team\nmetadata: {name: t, lables: {a: b}}", "metadata.lables"},
		{"envelope", "kind: Team\nmetadata: {name: t}\nstatus: {}", "status"},
		{"user metadata", "kind: User\nmetadata: {name: alice, labels: {a: b}}", "metadata.labels"},
		{"product field on another kind", "kind: Project\nmetadata: {name: p}\nspec: {team: t, costCenter: x}", "spec.costCenter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\n"+tc.src+"\n")
			if e.Field != tc.field || !strings.Contains(e.Error(), "unknown field") {
				t.Errorf("error = %v (field %q), want an unknown field at %q", e, e.Field, tc.field)
			}
		})
	}
}

func TestParseRefusesAPlainTextPassword(t *testing.T) {
	for _, tc := range []struct{ name, password, field string }{
		{"scalar", "password: hunter22", "spec.password"},
		{"value", "password: {value: hunter22}", "spec.password.value"},
		{"not a hash", "password: {hash: hunter22}", "spec.password.hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: alice}\nspec:\n  "+tc.password+"\n")
			if e.Field != tc.field || !errors.Is(e, manifest.ErrPlainPassword) {
				t.Errorf("error = %v at %q, want ErrPlainPassword at %q", e, e.Field, tc.field)
			}
		})
	}
	both := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: a}\nspec: {password: {hash: \"$2a$10$x\", valueFrom: {env: P}}}\n")
	if both.Field != "spec.password" {
		t.Errorf("hash and env together reported at %q", both.Field)
	}
}

func TestParseRefusesDocumentsOutsideThePackage(t *testing.T) {
	if e := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Policy\nmetadata: {name: p}\n"); e.Field != "kind" {
		t.Errorf("unknown kind reported at %q", e.Field)
	}
	if e := parseErr(t, "apiVersion: example.com/v1\nkind: Team\nmetadata: {name: t}\n"); e.Field != "apiVersion" {
		t.Errorf("foreign apiVersion reported at %q", e.Field)
	}
	if e := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {displayName: T}\n"); e.Field != "metadata.name" {
		t.Errorf("missing name reported at %q", e.Field)
	}
}

// Every kind derives its owner, so a document naming one is refused rather than silently overridden.
func TestParseRefusesMetadataOwner(t *testing.T) {
	e := parseErr(t, "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: t, owner: {kind: user}}\n")
	if e.Field != "metadata.owner" || !errors.Is(e, manifest.ErrOwnerDerived) {
		t.Errorf("error = %v at %q, want ErrOwnerDerived at metadata.owner", e, e.Field)
	}
}

// Document indexes count empty documents, so an error points at the right place in the file.
func TestParseCountsEmptyDocuments(t *testing.T) {
	docs, err := testRegistry(t).Parse(strings.NewReader("---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: a}\n---\n---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: b}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Index != 0 || docs[1].Index != 2 {
		t.Fatalf("docs = %+v", docs)
	}
}

// A product may accept the kinds under an apiVersion it published first.
func TestRegistryAcceptsAProductAPIVersion(t *testing.T) {
	cfg := testConfig()
	cfg.APIVersions = []string{"example.com/v1"}
	r := manifest.MustRegistry(cfg)
	docs, err := r.Parse(strings.NewReader("apiVersion: example.com/v1\nkind: Team\nmetadata: {name: t}\nspec: {costCenter: x}\n"))
	if err != nil || len(docs) != 1 || docs[0].APIVersion != "example.com/v1" {
		t.Fatalf("docs = %+v, err = %v", docs, err)
	}
}

func TestNewRegistryRefusesAFieldThatShadowsTheSpec(t *testing.T) {
	for _, fields := range []map[string][]manifest.Field{
		{manifest.KindProject: {{Name: "team"}}},
		{"Policy": {{Name: "x"}}},
		{manifest.KindTeam: {{Name: "a"}, {Name: "a"}}},
		{manifest.KindTeam: {{}}},
	} {
		if _, err := manifest.NewRegistry(manifest.Config{Fields: fields}); err == nil {
			t.Errorf("fields %v accepted", fields)
		}
	}
}
