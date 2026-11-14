package inference

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/adapters"
)

// aliasDispatchCatalog rebuilds the standard dispatch fixture so the host
// points at the given upstream URL with NoAuth (anonymous key — no secret
// resolution in tests) and the model declares an exact + wildcard alias.
func aliasDispatchCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	cat, pr := buildDispatchCatalog(t, "groq", adapters.OpenAI)
	snap := cat.Current()
	pointHostAt(t, cat, upstreamURL)

	models := snap.ModelsByName("test-model")
	if len(models) != 1 {
		t.Fatalf("fixture models: %d", len(models))
	}
	m := *models[0]
	m.Spec.Aliases = []string{"test-model[1m]", "test-modelx[*]"}
	if err := applyModel(t, cat, &m); err != nil {
		t.Fatalf("model upsert: %v", err)
	}
	return cat, pr
}

// An alias-addressed request byte-passes with its model rewritten to the alias's wire form: the declared string for an exact alias, however the caller spelled it, and the caller's raw string for a wildcard match.
func TestDispatch_AliasBytePass_VerbatimWireName(t *testing.T) {
	upURL, last := answeringUpstream(t, "application/json", `{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	cat, pr := aliasDispatchCatalog(t, upURL)
	d := buildRunnableDeps(t, cat)

	for name, tc := range map[string]struct{ send, wantWire string }{
		"exact alias, declared spelling":       {"test-model[1m]", "test-model[1m]"},
		"exact alias, caller variant spelling": {"TEST-MODEL.1M", "test-model[1m]"},
		"wildcard alias, raw goes through":     {"test-modelx[exp-2027]", "test-modelx[exp-2027]"},
		"no alias, snapshot upstream":          {"test-model", "test-model"},
	} {
		t.Run(name, func(t *testing.T) {
			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"` + tc.send + `","stream":false}`), ModelName: tc.send})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if _, body := last(); body != `{"model":"`+tc.wantWire+`","stream":false}` {
				t.Errorf("upstream body = %s, want the model %q and nothing else changed", body, tc.wantWire)
			}
		})
	}
}
