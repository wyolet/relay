package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/manifest"
)

// A product field is stored as an attribute under its own key, resolved through the product's resolver, and rendered back by name.
func TestProductFieldsRideInAttributes(t *testing.T) {
	e := newEnv(t)
	src := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: t}\n---\n" +
		"apiVersion: auth.wyolet.dev/v1alpha1\nkind: Project\nmetadata: {name: p}\nspec: {team: t}\n---\n" +
		"apiVersion: auth.wyolet.dev/v1alpha1\nkind: ServiceAccount\nmetadata: {name: sa}\nspec: {project: p, budget: q4}\n"
	docs := e.parse(t, src)
	if string(docs[2].Fields["budget"]) != `"q4"` {
		t.Fatalf("fields = %s", docs[2].Fields)
	}
	e.apply(t, src, manifest.Options{})
	sas, _ := e.store.Stores().ServiceAccounts.List(context.Background())
	if got := sas[0].Spec.Attributes; len(got) != 1 || string(got["budgetId"]) != `"budget-q4"` {
		t.Fatalf("attributes = %s", got)
	}
	d, err := e.reg.FromServiceAccount(sas[0], storeNames(t, e))
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Fields["budget"]) != `"q4"` {
		t.Errorf("rendered fields = %s", d.Fields)
	}

	bad := strings.Replace(src, "budget: q4", "budget: q9", 1)
	_, err = e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, bad), e.opts)
	var ref *manifest.RefError
	if pe := asError(t, err); pe.Field != "spec.budget" || !errors.As(err, &ref) || ref.Kind != "budget" {
		t.Errorf("unresolved product reference = %v", err)
	}
}

// A field with no hooks is stored and rendered as written, any JSON shape.
func TestAFieldWithoutHooksIsStoredVerbatim(t *testing.T) {
	cfg := testConfig()
	cfg.Fields[manifest.KindGroup] = []manifest.Field{{Name: "quota"}}
	r := manifest.MustRegistry(cfg)
	docs, err := r.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Group\nmetadata: {name: g}\nspec: {quota: {seats: 5, tier: gold}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.ToGroup(&docs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	var quota map[string]any
	if err := json.Unmarshal(g.Spec.Attributes["quota"], &quota); err != nil || quota["seats"] != 5.0 {
		t.Fatalf("attribute = %s (%v)", g.Spec.Attributes["quota"], err)
	}
	back, err := r.FromGroup(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := manifest.Render([]*manifest.Document{back})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "quota:\n    seats: 5\n    tier: gold") {
		t.Errorf("rendered:\n%s", out)
	}
}

func TestJSONSchemaCarriesTheVocabularyAndTheFields(t *testing.T) {
	r := testRegistry(t)
	all, err := r.JSONSchemas()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(manifest.Kinds()) {
		t.Fatalf("%d schemas, want one per kind", len(all))
	}
	var role struct {
		Properties struct {
			Spec struct {
				Properties struct {
					Rules struct {
						Items struct {
							Properties struct {
								Verbs struct {
									Items struct {
										Enum []string `json:"enum"`
									} `json:"items"`
								} `json:"verbs"`
							} `json:"properties"`
						} `json:"items"`
					} `json:"rules"`
				} `json:"properties"`
				AdditionalProperties bool `json:"additionalProperties"`
			} `json:"spec"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(all[manifest.KindRole], &role); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(role.Properties.Spec.Properties.Rules.Items.Properties.Verbs.Items.Enum, ","); got != "*,get,list,update" {
		t.Errorf("verb enum = %s", got)
	}
	if !strings.Contains(string(all[manifest.KindTeam]), `"costCenter"`) || strings.Contains(string(all[manifest.KindProject]), `"costCenter"`) {
		t.Errorf("product field placed wrong:\n%s", all[manifest.KindTeam])
	}
	if !strings.Contains(string(all[manifest.KindUser]), `Omit to keep the stored password`) {
		t.Errorf("user schema does not describe keeping an omitted password:\n%s", all[manifest.KindUser])
	}
	if strings.Contains(string(all[manifest.KindTeam]), `"owner"`) {
		t.Errorf("team schema offers metadata.owner:\n%s", all[manifest.KindTeam])
	}
	if _, err := r.JSONSchema("Policy"); err == nil {
		t.Error("schema for an unknown kind")
	}
}
