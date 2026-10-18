package openai

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Tool-item anchors and CacheConfig.History have no OpenAI wire form: both shapes ignore them without error.
func TestSerializeRequest_ToolAnchorsAndHistoryAreNoOps(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	req := &v1.Request{
		Model:       v1.ModelRefs{"gpt-5-5"},
		CacheConfig: &v1.CacheConfig{History: true, Key: "session-1"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
			&v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
			&v1.FunctionCallOutput{CallID: "c1", Output: "ok", CacheConfig: anchor},
		},
	}
	for name, tr := range map[string]v1.Translator{"cc": CCTranslator{}, "responses": ResponsesTranslator{}} {
		b, err := tr.SerializeRequest(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := string(b)
		if strings.Contains(s, "cache_control") || strings.Contains(s, "cache_config") || strings.Contains(s, "history") {
			t.Errorf("%s: cache vocabulary leaked: %s", name, s)
		}
		if !strings.Contains(s, "session-1") {
			t.Errorf("%s: prompt cache key lost: %s", name, s)
		}
	}
}
