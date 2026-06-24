package catalog

import (
	"strings"
	"testing"
)

// twoHostJSON: the same bare model "claude" served by a pin-skipped host
// (amazon-bedrock) and a normal host (anthropic). Bare "claude" is ambiguous.
const twoHostJSON = `{
  "version":"test","hosts":[
    {"name":"amazon-bedrock","baseURL":"https://bedrock","models":[
      {"model":"claude","adapter":"anthropic","upstream":"anthropic.claude-v2","providers":["anthropic"]}]},
    {"name":"anthropic","baseURL":"https://api.anthropic.com","models":[
      {"model":"claude","adapter":"anthropic","upstream":"claude-3","providers":["anthropic"]}]}
  ]}`

// Every host gets a resolvable @host pin, including hosts whose names used to
// be pin-skipped (amazon-bedrock/google-vertex). The ambiguity error advertises
// @host pins as the fix, so every advertised pin must actually resolve.
func TestResolve_EveryAmbiguityCandidateResolves(t *testing.T) {
	ic, err := LoadBytes([]byte(twoHostJSON))
	if err != nil {
		t.Fatal(err)
	}

	_, _, ambErr := ic.Resolve("claude")
	if ambErr == nil {
		t.Fatal("expected ambiguity error for bare 'claude'")
	}
	if !strings.Contains(ambErr.Error(), "claude@amazon-bedrock") {
		t.Fatalf("ambiguity error does not advertise the bedrock pin: %v", ambErr)
	}

	// Both advertised pins must resolve — including the formerly-skipped one.
	for _, ref := range []string{"claude@amazon-bedrock", "claude@anthropic"} {
		if _, _, err := ic.Resolve(ref); err != nil {
			t.Errorf("advertised pin %q does not resolve: %v", ref, err)
		}
	}
}

// The non-skipped side of the same ambiguity resolves fine via pin.
func TestResolve_NonSkippedPin_Works(t *testing.T) {
	ic, _ := LoadBytes([]byte(twoHostJSON))
	b, h, err := ic.Resolve("claude@anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "anthropic" || b.Name != "claude-3" {
		t.Fatalf("got host=%q upstream=%q", h.Name, b.Name)
	}
}

// normRef maps '@', '/', and '-' to the same separator, so distinct ref
// syntaxes collapse. Verify "model@host" and "model-host" collide — a caller
// who happens to name a model "gpt-host" could shadow a pin. Documents the
// normalization's blast radius.
func TestResolve_NormalizationCollision(t *testing.T) {
	const j = `{"version":"t","hosts":[
      {"name":"host","baseURL":"https://x","models":[
        {"model":"gpt","adapter":"openai","upstream":"gpt","providers":["openai"]}]}]}`
	ic, _ := LoadBytes([]byte(j))
	// "gpt@host" is the pin; "gpt-host" normalizes identically and hits the pin too.
	if _, _, err := ic.Resolve("gpt@host"); err != nil {
		t.Fatalf("pin form failed: %v", err)
	}
	if _, _, err := ic.Resolve("gpt-host"); err != nil {
		t.Fatalf("dash form normalizes to the same key but failed: %v", err)
	}
}

// provider/model@host (fully qualified pin) resolves.
func TestResolve_ProviderQualifiedPin(t *testing.T) {
	const j = `{"version":"t","hosts":[
      {"name":"openai","baseURL":"https://x","models":[
        {"model":"gpt-4o","adapter":"openai","upstream":"gpt-4o-2024","providers":["openai"]}]},
      {"name":"azure","baseURL":"https://y","models":[
        {"model":"gpt-4o","adapter":"openai","upstream":"gpt-4o-dep","providers":["openai"]}]}]}`
	ic, _ := LoadBytes([]byte(j))
	b, h, err := ic.Resolve("openai/gpt-4o@azure")
	if err != nil {
		t.Fatalf("provider/model@host pin failed: %v", err)
	}
	if h.Name != "azure" || b.Name != "gpt-4o-dep" {
		t.Fatalf("got host=%q upstream=%q", h.Name, b.Name)
	}
}

// Empty / whitespace ref is rejected, not silently matched.
func TestResolve_EmptyRef(t *testing.T) {
	ic, _ := LoadBytes([]byte(twoHostJSON))
	for _, ref := range []string{"", "   ", "///", "@@@"} {
		if _, _, err := ic.Resolve(ref); err == nil {
			t.Errorf("ref %q should error", ref)
		}
	}
}
