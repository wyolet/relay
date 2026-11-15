package openai

import (
	"strings"
	"testing"
)

// Each case parses a Chat Completions body to canonical and serializes that back to the wire; the recorded chat-* goldens cover the plain request forms.
func TestCCParseRequest(t *testing.T) {
	cases := []struct {
		name, body, canon, wire string
	}{
		{
			name: "system messages after the first become developer messages",
			body: `{"model":"m","messages":[{"role":"system","content":"A"},{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"B"},{"type":"text","text":"C"}]},{"role":"developer","content":"D"}]}`,
			canon: `{"model":"m","instructions":"A","output_mode":"sync","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"BC"}]},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"D"}]}]}`,
			wire: `{"model":"m","messages":[{"role":"system","content":"A"},{"role":"user","content":"hi"},{"role":"system","content":"BC"},{"role":"system","content":"D"}]}`,
		},
		{
			name: "user content parts",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"https://x/a.png","detail":"low"}},
				{"type":"file","file":{"file_id":"file-1","filename":"a.pdf"}},
				{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}]}]}`,
			canon: `{"model":"m","output_mode":"sync","input":[{"type":"message","role":"user","content":[
				{"type":"input_text","text":"look"},
				{"type":"input_image","image_url":"https://x/a.png","detail":"low"},
				{"type":"input_file","file_id":"file-1","filename":"a.pdf"}]}]}`,
			wire: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"https://x/a.png","detail":"low"}},
				{"type":"file","file":{"file_id":"file-1","filename":"a.pdf"}}]}]}`,
		},
		{
			name: "assistant history with refusal, tool calls and tool results",
			body: `{"model":"m","messages":[
				{"role":"assistant","content":"Sure","refusal":"but no","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
				{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"shot"},{"type":"image_url","image_url":{"url":"https://x/s.png"}}]},
				{"role":"assistant","content":null}]}`,
			canon: `{"model":"m","output_mode":"sync","input":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sure"},{"type":"output_text","text":"but no"}]},
				{"type":"function_call","id":"call_1","call_id":"call_1","name":"f","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"ab"},
				{"type":"function_call_output","call_id":"call_1","content":[{"type":"input_text","text":"shot"},{"type":"input_image","image_url":"https://x/s.png"}]},
				{"type":"message","role":"assistant","content":null}]}`,
			wire: `{"model":"m","messages":[
				{"role":"assistant","content":"Surebut no","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ab"},
				{"role":"tool","tool_call_id":"call_1","content":"shot"},
				{"role":"assistant","content":null}]}`,
		},
		{
			name: "legacy function calls pair with their function results under minted ids",
			body: `{"model":"m","messages":[
				{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}},
				{"role":"function","name":"f","content":"sunny"},
				{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{\"d\":2}"}},
				{"role":"function","name":"f","content":"rain"}]}`,
			canon: `{"model":"m","output_mode":"sync","input":[
				{"type":"function_call","id":"f_0","call_id":"f_0","name":"f","arguments":"{}"},
				{"type":"function_call_output","call_id":"f_0","output":"sunny"},
				{"type":"function_call","id":"f_2","call_id":"f_2","name":"f","arguments":"{\"d\":2}"},
				{"type":"function_call_output","call_id":"f_2","output":"rain"}]}`,
			wire: `{"model":"m","messages":[
				{"role":"assistant","content":null,"tool_calls":[{"id":"f_0","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"f_0","content":"sunny"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"f_2","type":"function","function":{"name":"f","arguments":"{\"d\":2}"}}]},
				{"role":"tool","tool_call_id":"f_2","content":"rain"}]}`,
		},
		{
			name: "sampling with stop string and max_completion_tokens over max_tokens",
			body: `{"model":"m","messages":[],"temperature":0.5,"top_p":0.9,"max_tokens":10,"max_completion_tokens":20,"frequency_penalty":0.1,"presence_penalty":0.2,"seed":7,"stop":"END","reasoning_effort":"low"}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"model_config":{"m":{
				"sampling":{"temperature":0.5,"top_p":0.9,"max_tokens":20,"frequency_penalty":0.1,"presence_penalty":0.2,"seed":7,"stop":["END"]},
				"reasoning":{"effort":"low"}}}}`,
			wire: `{"model":"m","messages":null,"temperature":0.5,"top_p":0.9,"max_tokens":20,"frequency_penalty":0.1,"presence_penalty":0.2,"seed":7,"stop":["END"],"reasoning_effort":"low"}`,
		},
		{
			name: "tools with the flat tool_choice form and no parameters",
			body: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","description":"d"}}],"tool_choice":{"type":"function","name":"f"},"parallel_tool_calls":false}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"tools":{
				"definitions":[{"type":"function","name":"f","description":"d","parameters":{}}],
				"choice":{"type":"function","name":"f"},"parallel":false}}`,
			wire: `{"model":"m","messages":null,"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{}}}],
				"tool_choice":{"type":"function","function":{"name":"f"}},"parallel_tool_calls":false}`,
		},
		{
			name: "string tool_choice",
			body: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"required"}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"tools":{
				"definitions":[{"type":"function","name":"f","parameters":{"type":"object"}}],"choice":"required"}}`,
			wire: `{"model":"m","messages":null,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"required"}`,
		},
		{
			name: "allowed_tools tool_choice",
			body: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"g"}}],
				"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"g"}}]}}}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"tools":{
				"definitions":[{"type":"function","name":"f","parameters":{}},{"type":"function","name":"g","parameters":{}}],
				"choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"g"}]}}}`,
			wire: `{"model":"m","messages":null,"tools":[{"type":"function","function":{"name":"f","parameters":{}}},{"type":"function","function":{"name":"g","parameters":{}}}],
				"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"g"}}]}}}`,
		},
		{
			name: "json_schema response format with verbosity",
			body: `{"model":"m","messages":[],"verbosity":"high","response_format":{"type":"json_schema","json_schema":{"name":"out","description":"d","schema":{"type":"object"},"strict":true}}}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"model_config":{"m":{"output":{"verbosity":"high",
				"format":{"type":"json_schema","name":"out","description":"d","schema":{"type":"object"},"strict":true}}}}}`,
			wire: `{"model":"m","messages":null,"verbosity":"high","response_format":{"type":"json_schema","json_schema":{"name":"out","description":"d","schema":{"type":"object"},"strict":true}}}`,
		},
		{
			name:  "json_object response format",
			body:  `{"model":"m","messages":[],"response_format":{"type":"json_object"}}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"model_config":{"m":{"output":{"format":{"type":"json_object"}}}}}`,
			wire:  `{"model":"m","messages":null,"response_format":{"type":"json_object"}}`,
		},
		{
			name:  "text response format is the default and goes unsent",
			body:  `{"model":"m","messages":[],"response_format":{"type":"text"}}`,
			canon: `{"model":"m","output_mode":"sync","input":null,"model_config":{"m":{"output":{"format":{"type":"text"}}}}}`,
			wire:  `{"model":"m","messages":null}`,
		},
		{
			name:  "in_memory cache retention maps to a 5m TTL",
			body:  `{"model":"m","messages":[],"prompt_cache_key":"k","prompt_cache_retention":"in_memory","stream":true}`,
			canon: `{"model":"m","output_mode":"stream","input":null,"cache_config":{"key":"k","ttl":"5m"}}`,
			wire:  `{"model":"m","messages":null,"prompt_cache_key":"k","prompt_cache_retention":"in_memory","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name:  "unknown cache retention is dropped",
			body:  `{"model":"m","messages":[],"prompt_cache_retention":"forever"}`,
			canon: `{"model":"m","output_mode":"sync","input":null}`,
			wire:  `{"model":"m","messages":null}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := CCTranslator{}.ParseRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, mustJSON(req), tc.canon)
			wire, err := CCTranslator{}.SerializeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, wire, tc.wire)
		})
	}
}

// Valid Chat Completions input must reach the re-serialized wire or fail the parse, never vanish (rule 11).
func TestCCParseRequest_NoSilentDrop(t *testing.T) {
	cases := []struct{ name, bug, body, keep string }{
		{
			name: "custom tool",
			body: `{"model":"m","messages":[],"tools":[{"type":"custom","custom":{"name":"grammar"}}]}`,
			keep: `"grammar"`,
		},
		{
			name: "allowed_tools tool_choice",
			body: `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"f"}}]}}}`,
			keep: `"allowed_tools":{`,
		},
		{
			name: "legacy function role message",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"function","name":"f","content":"sunny"}]}`,
			keep: `sunny`,
		},
		{
			name: "assistant refusal content part",
			body: `{"model":"m","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"I won't"}]}]}`,
			keep: `I won't`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.bug != "" {
				t.Skip("bug: " + tc.bug)
			}
			req, err := CCTranslator{}.ParseRequest([]byte(tc.body))
			if err != nil {
				return
			}
			wire, err := CCTranslator{}.SerializeRequest(req)
			if err != nil {
				return
			}
			if !strings.Contains(string(wire), tc.keep) {
				t.Errorf("%s lost without an error: %s", tc.keep, wire)
			}
		})
	}
}

func TestCCParseRequest_Errors(t *testing.T) {
	for name, body := range map[string]string{
		"invalid JSON":          `{"model":`,
		"missing model":         `{"messages":[{"role":"user","content":"x"}]}`,
		"mistyped user content": `{"model":"m","messages":[{"role":"user","content":5}]}`,
		"mistyped tool content": `{"model":"m","messages":[{"role":"tool","tool_call_id":"c","content":{}}]}`,
		"mistyped tool_choice":  `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":5}`,
		"allowed custom tool":   `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"custom","custom":{"name":"g"}}]}}}`,
		"empty allowed list":    `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}}`,
		"allowed mode none":     `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"none","tools":[{"type":"function","function":{"name":"f"}}]}}}`,
	} {
		if req, err := (CCTranslator{}).ParseRequest([]byte(body)); err == nil {
			t.Errorf("%s: want an error, got %s", name, mustJSON(req))
		}
	}
}
