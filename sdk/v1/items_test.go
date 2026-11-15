package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUnmarshalItem(t *testing.T) {
	anchor := &ItemCacheConfig{Anchor: true}
	tests := []struct {
		name string
		wire string
		want Item
		// rewire is what the item marshals back to; empty means wire itself.
		rewire string
	}{
		{
			name:   "message with string content becomes one input_text part",
			wire:   `{"type":"message","role":"user","content":"hello"}`,
			want:   &Message{Role: RoleUser, Content: []Part{&TextPart{Text: "hello"}}},
			rewire: `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`,
		},
		{
			name: "message with every field",
			wire: `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}],"cache_config":{"anchor":true},"hoist":true,"provider_data":{"sig":"abc"}}`,
			want: &Message{ID: "msg_1", Status: StatusCompleted, Role: RoleAssistant, Content: []Part{&OutputTextPart{Text: "ok"}},
				CacheConfig: anchor, Hoist: true, ProviderData: json.RawMessage(`{"sig":"abc"}`)},
		},
		{
			name: "message with mixed parts keeps their order",
			wire: `{"type":"message","role":"developer","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"u"},{"type":"input_text","text":"b"}]}`,
			want: &Message{Role: RoleDeveloper, Content: []Part{&TextPart{Text: "a"}, &ImagePart{ImageURL: "u"}, &TextPart{Text: "b"}}},
		},
		{
			name:   "message without content",
			wire:   `{"type":"message","role":"system"}`,
			want:   &Message{Role: RoleSystem},
			rewire: `{"type":"message","role":"system","content":null}`,
		},
		{
			name: "message with empty content array",
			wire: `{"type":"message","role":"user","content":[]}`,
			want: &Message{Role: RoleUser, Content: []Part{}},
		},
		{
			name: "function_call with every field",
			wire: `{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}","status":"completed","cache_config":{"anchor":true},"provider_data":{"k":1}}`,
			want: &FunctionCall{ID: "fc_1", CallID: "call_abc", Name: "get_weather", Arguments: `{"city":"NYC"}`, Status: StatusCompleted,
				CacheConfig: anchor, ProviderData: json.RawMessage(`{"k":1}`)},
		},
		{
			name: "function_call minimal keeps required members on the wire",
			wire: `{"type":"function_call","call_id":"","name":"","arguments":""}`,
			want: &FunctionCall{},
		},
		{
			name: "function_call_output string form",
			wire: `{"type":"function_call_output","call_id":"call_abc","output":"sunny","cache_config":{"anchor":true}}`,
			want: &FunctionCallOutput{CallID: "call_abc", Output: "sunny", CacheConfig: anchor},
		},
		{
			name: "function_call_output content form",
			wire: `{"type":"function_call_output","call_id":"call_abc","content":[{"type":"input_text","text":"done"},{"type":"input_image","image_url":"u"}]}`,
			want: &FunctionCallOutput{CallID: "call_abc", Content: []Part{&TextPart{Text: "done"}, &ImagePart{ImageURL: "u"}}},
		},
		{
			name:   "function_call_output null output and content",
			wire:   `{"type":"function_call_output","call_id":"c","output":null,"content":null}`,
			want:   &FunctionCallOutput{CallID: "c"},
			rewire: `{"type":"function_call_output","call_id":"c"}`,
		},
		{
			name: "reasoning with every field",
			wire: `{"type":"reasoning","id":"rs_1","summary":[{"text":"step 1"},{"text":"step 2"}],"content":"raw","status":"completed","provider_data":{"encrypted":"xyz"}}`,
			want: &Reasoning{ID: "rs_1", Summary: []SummaryText{{Text: "step 1"}, {Text: "step 2"}}, Content: "raw", Status: StatusCompleted,
				ProviderData: json.RawMessage(`{"encrypted":"xyz"}`)},
		},
		{
			name: "reasoning minimal",
			wire: `{"type":"reasoning"}`,
			want: &Reasoning{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UnmarshalItem([]byte(tc.wire))
			if err != nil {
				t.Fatalf("UnmarshalItem: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("item (-want +got):\n%s", diff)
			}
			if got.ItemType() != tc.want.ItemType() {
				t.Errorf("ItemType = %q, want %q", got.ItemType(), tc.want.ItemType())
			}
			rewire := tc.rewire
			if rewire == "" {
				rewire = tc.wire
			}
			wantJSON(t, got, rewire)
		})
	}
}

func TestUnmarshalItem_Errors(t *testing.T) {
	tests := []struct {
		name, wire, want string
	}{
		{"not an object", `[]`, "item: json: cannot unmarshal array…"},
		{"missing type", `{"role":"user"}`, `unsupported item type ""`},
		{"vendor built-in call", `{"type":"web_search_call"}`, `unsupported item type "web_search_call"`},
		{"compaction", `{"type":"compaction"}`, `unsupported item type "compaction"`},
		{"type is case-sensitive", `{"type":"Message","role":"user"}`, `unsupported item type "Message"`},
		{"message with unknown part", `{"type":"message","role":"user","content":[{"type":"audio"}]}`, `message item: unsupported part type "audio"`},
		{"message with object content", `{"type":"message","role":"user","content":{}}`, "message item: content array: json: cannot unmarshal object…"},
		{"message with numeric role", `{"type":"message","role":1}`, "message item: json: cannot unmarshal number…"},
		{"function_call with numeric name", `{"type":"function_call","name":1}`, "function_call item: json: cannot unmarshal number…"},
		{"function_call_output with object output", `{"type":"function_call_output","output":{}}`, "function_call_output item: function_call_output.output: json: cannot unmarshal object…"},
		{"function_call_output with unknown part", `{"type":"function_call_output","content":[{"type":"x"}]}`, `function_call_output item: unsupported part type "x"`},
		{"function_call_output with numeric call_id", `{"type":"function_call_output","call_id":1}`, "function_call_output item: json: cannot unmarshal number…"},
		{"reasoning with string summary", `{"type":"reasoning","summary":"x"}`, "reasoning item: json: cannot unmarshal string…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UnmarshalItem([]byte(tc.wire))
			if err == nil {
				t.Fatalf("UnmarshalItem = %#v, want an error", got)
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestUnmarshalItems(t *testing.T) {
	got, err := UnmarshalItems([]byte(`[
		{"type":"message","role":"user","content":"what is the weather?"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"},
		{"type":"reasoning","id":"rs_1"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{
		&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "what is the weather?"}}},
		&FunctionCall{ID: "fc_1", CallID: "call_1", Name: "weather", Arguments: "{}"},
		&FunctionCallOutput{CallID: "call_1", Output: "sunny"},
		&Reasoning{ID: "rs_1"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("items (-want +got):\n%s", diff)
	}

	for _, tc := range []struct{ name, wire, want string }{
		{"not an array", `{"type":"message"}`, "input array: json: cannot unmarshal object…"},
		{"second element unsupported", `[{"type":"reasoning"},{"type":"mcp_call"}]`, `unsupported item type "mcp_call"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := UnmarshalItems([]byte(tc.wire))
			if items != nil {
				t.Errorf("items = %v, want nil on error", items)
			}
			wantErr(t, err, tc.want)
		})
	}

	if got, err := UnmarshalItems([]byte(`[]`)); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty array = %v, %v; want an empty non-nil slice", got, err)
	}
}
