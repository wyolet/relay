package role

import "testing"

// Revoking a token is authorized as tokens.revoke, so the verb has to be in
// the vocabulary a rule may name — and the scope admins who mint tokens are
// the ones who have to be able to take them back.
func TestScopeAdminsMayMintAndRevokeTokens(t *testing.T) {
	if !inSet(Verbs, "revoke") {
		t.Fatal("revoke is not in the verb vocabulary")
	}
	builtins, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	byName := map[string]*Role{}
	for _, r := range builtins {
		byName[r.Meta.Name] = r
	}
	for _, name := range []string{"team-admin", "project-admin"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("%s is not a built-in", name)
		}
		for _, verb := range []string{"mint", "revoke"} {
			if !r.Allows("tokens", verb) {
				t.Errorf("%s does not allow tokens.%s", name, verb)
			}
		}
	}
	// Every rule the file declares must survive validation, which is what
	// rejects a verb the vocabulary does not carry.
	for _, r := range builtins {
		r.Meta.ID = "00000000-0000-7000-8000-000000000000"
		if err := r.Validate(); err != nil {
			t.Errorf("built-in %q does not validate: %v", r.Meta.Name, err)
		}
	}
}

// A rule set that differs from the embedded file must be detected, or a rule
// added in a release never reaches an upgraded deployment.
func TestSameRules(t *testing.T) {
	a := []Rule{{Kinds: []string{"tokens"}, Verbs: []string{"mint"}}}
	if !sameRules(a, []Rule{{Kinds: []string{"tokens"}, Verbs: []string{"mint"}}}) {
		t.Error("identical rule sets reported as different")
	}
	if sameRules(a, []Rule{{Kinds: []string{"tokens"}, Verbs: []string{"mint", "revoke"}}}) {
		t.Error("an added verb was not detected")
	}
	if sameRules(a, nil) {
		t.Error("a dropped rule was not detected")
	}
}
