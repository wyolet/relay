package control

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/schemas"
)

// A closed set in the code is only a closed set to an editor if the schema
// says so; without the enum a typo in a role rule is caught at apply time
// instead of while writing the YAML.
func TestGeneratedSchemasCarryTheClosedSets(t *testing.T) {
	raw, err := schemas.FS.ReadFile("v1alpha2/Role.schema.json")
	if err != nil {
		t.Fatalf("read Role schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rule := ruleSchema(t, doc)
	for field, want := range map[string][]string{"kinds": role.Kinds, "verbs": role.Verbs} {
		prop, ok := rule[field].(map[string]any)
		if !ok {
			t.Fatalf("rule has no %s property", field)
		}
		if prop["minItems"] == nil {
			t.Errorf("%s has no minItems: an empty rule grants nothing and should not validate", field)
		}
		items, _ := prop["items"].(map[string]any)
		enum, _ := items["enum"].([]any)
		if len(enum) != len(want) {
			t.Fatalf("%s enum has %d values, want the %d in the vocabulary", field, len(enum), len(want))
		}
		got := map[string]bool{}
		for _, v := range enum {
			got[v.(string)] = true
		}
		for _, v := range want {
			if !got[v] {
				t.Errorf("%s enum is missing %q", field, v)
			}
		}
	}
}

// ruleSchema digs the rule object out of the generated $defs.
func ruleSchema(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	defs, _ := doc["$defs"].(map[string]any)
	for _, def := range defs {
		obj, ok := def.(map[string]any)
		if !ok {
			continue
		}
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasKinds := props["kinds"]; hasKinds {
			if _, hasVerbs := props["verbs"]; hasVerbs {
				return props
			}
		}
	}
	t.Fatal("no rule definition in the Role schema")
	return nil
}
