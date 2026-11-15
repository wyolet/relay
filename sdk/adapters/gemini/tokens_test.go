package gemini

import (
	"maps"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/sdk/usage"
)

// Cache writes are prompt tokens, so promptTokenCount must include them.
func TestCanonicalUsageToGemini_CacheCreationInPromptTokens(t *testing.T) {
	got := canonicalUsageToGemini(usage.Tokens{"input": 300, "cache_read": 100, "cache_creation": 200, "output": 50, "reasoning": 10})
	want := map[string]int64{"promptTokenCount": 600, "candidatesTokenCount": 40, "cachedContentTokenCount": 100, "thoughtsTokenCount": 10}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("usageMetadata (-want +got):\n%s", d)
	}
}

func TestExtractTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want usage.Tokens
	}{
		{
			"buffered: input excludes cached, output includes thoughts",
			` {"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"cachedContentTokenCount":20,"thoughtsTokenCount":30}}`,
			usage.Tokens{"input": 80, "output": 80, "cache_read": 20, "reasoning": 30},
		},
		{
			"stream keeps the last frame carrying usage",
			"data: {\"usageMetadata\":{\"promptTokenCount\":3}}\n\n" +
				"data: {\"usageMetadata\":{\"promptTokenCount\":90,\"candidatesTokenCount\":11,\"cachedContentTokenCount\":25,\"thoughtsTokenCount\":5}}\n\n" +
				"data: {\"candidates\":[]}\n\ndata: [DONE]\n\n",
			usage.Tokens{"input": 65, "output": 16, "cache_read": 25, "reasoning": 5},
		},
		{
			"multi-line data",
			"data: {\"candidates\":[],\ndata: \"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5}}\n\n",
			usage.Tokens{"input": 3, "output": 5},
		},
		{"frames without a JSON object are skipped", "data:  \n\ndata: [1]\n\ndata: {\"usageMetadata\":{\"promptTokenCount\":3}}\n\n", usage.Tokens{"input": 3}},
		{"mistyped count drops the usage", `{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":"x"}}`, nil},
		{"negative counts read as absent", `{"usageMetadata":{"promptTokenCount":-2,"candidatesTokenCount":-1,"cachedContentTokenCount":-1,"thoughtsTokenCount":-1}}`, nil},
		{"all counts zero", `{"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`, nil},
		{"no usage", `{"candidates":[]}`, nil},
		{"blank body", " \n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractTokens([]byte(c.body))
			if !maps.Equal(got, c.want) || (got == nil) != (c.want == nil) {
				t.Errorf("tokens %v, want %v", got, c.want)
			}
		})
	}
}
