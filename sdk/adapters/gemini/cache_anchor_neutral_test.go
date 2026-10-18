package gemini

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Tool-item anchors and CacheConfig.History have no Gemini wire form: they are ignored without error.
func TestSerializeRequest_ToolAnchorsAndHistoryAreNoOps(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	b, err := (GeminiTranslator{}).SerializeRequest(&v1.Request{
		Model:       v1.ModelRefs{"gemini-3-8-flash"},
		CacheConfig: &v1.CacheConfig{History: true},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
			&v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
			&v1.FunctionCallOutput{CallID: "c1", Output: "ok", CacheConfig: anchor},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "cache") {
		t.Errorf("cache vocabulary leaked: %s", s)
	}
}
