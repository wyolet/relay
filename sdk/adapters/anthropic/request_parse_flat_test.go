package anthropic

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// bs spells the backslash of a JSON \u escape in the cases below.
const bs = `\`

func flatCaseBody(members string) []byte {
	return []byte(`{"model":"m","max_tokens":8,` + members + `}`)
}

// Bodies parseRequestFlat must decode itself, each covering a mapping or an
// encoding/json behaviour it has to reproduce.
var flatAcceptCases = []string{
	`"messages":[{"role":"user","content":"hi"}]`,
	`"Messages":[{"ROLE":"user","Content":[{"TYPE":"text","Text":"folded keys"}]}]`,
	`"messages":[{"role":"user","content":"esc \" \\ \/ \b \f \n \r \t é ` + bs + `u00e9 ` + bs + `ud83d` + bs + `ude00 ` + bs + `ud800 \\u0041"}]`,
	"\"messages\":[{\"role\":\"user\",\"content\":\"raw invalid utf-8 \xff\xfe end\"}]",
	`"system":"plain system","messages":[]`,
	`"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":""},{"type":"image","text":"skipped"},{"type":"text","text":"b"}]`,
	`"system":{"type":"text"},"messages":null`,
	`"system":5,"tools":[],"tool_choice":{"type":"any"}`,
	`"tools":[{"name":"f","description":"d","input_schema":{"type":"object" , "properties":{}}},{"name":"g"},{"name":"h","input_schema":null,"cache_control":{"type":"ephemeral"}},{"type":"web_search_20250305","name":"web_search"},{"type":"function","name":"i"}],"tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true}`,
	`"tools":[{"name":"f","cache_control":null}],"tool_choice":null`,
	`"temperature":0.5,"top_p":0.9,"top_k":40,"stop_sequences":["x"],"stream":true,"metadata":{"user_id":"u"},"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"},"unknown_member":[1,{"a":2}]`,
	`"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"thinking","thinking":"unsigned"},{"type":"redacted_thinking","data":"x"},{"type":"tool_use","id":"t1","name":"f","input":{"a" : [1, 2]},"cache_control":{"type":"ephemeral"}},{"type":"tool_use","id":"t2","name":"g"},{"type":"tool_use","id":"t3","name":"h","input":null}]}]`,
	`"messages":[{"role":"assistant","content":null},{"role":"assistant"},{"role":"assistant","content":"said"},{"role":"assistant","content":[]},{"role":"assistant","content":[{"type":"text","text":"t","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"url","url":"u"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]`,
	`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"see"},{"type":"image","source":{"type":"base64","data":"QQ=="}}],"cache_control":{"type":"ephemeral"}},{"type":"tool_result","tool_use_id":"t4","content":null},{"type":"tool_result","tool_use_id":"t5","content":{"type":"text"}},{"type":"tool_result","tool_use_id":"t6"},{"type":"tool_result","tool_use_id":"t7","content":[{"type":"tool_result","content":"nested"}],"is_error":true},{"type":"text","text":"and","cache_control":{"type":"ephemeral"}}]}]`,
	`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ=="}},{"type":"image","source":{"type":"url","url":""}},{"type":"image","source":null},{"type":"image"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QQ=="},"title":"t"},{"type":"document","source":{"type":"url","url":"https://x"}},{"type":"document","source":{"type":"file","file_id":"f"}},{"type":"document","source":{"type":"text","data":"x"}},{"type":null,"text":"untyped","cache_control":null}]}]`,
	`"messages":[{"role":"user","content":"<system>\nbe brief\n</system>"},{"role":"user","content":[{"type":"text","text":"<system>\nwrapped\n</system>","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t"},{"type":"text","text":"<system>\nnot alone\n</system>"}]}]`,
	`"messages":[{"role":"system","content":[{"type":"text","text":"mid","cache_control":{"type":"ephemeral"}}]},{"role":"system","content":[]},{"role":"system"},{"role":"developer","content":"unknown role"},{"role":"developer","content":5},{"role":null,"content":[{"type":"tool_use","id":"x"}]},{"content":"no role"}]`,
	`"messages":[{"role":"user","content":[]},{"role":"user","content":null},{"role":"user"},{"role":"user","content":[{"type":"text","text":"x","citations":[{"a":1}]}]}]`,
}

// Bodies parseRequestFlat must leave to parseRequestLayered: each is invalid,
// or the layered probes treat some part of it in a way a single read of each
// field does not reproduce.
var flatDeclineCases = []string{
	`"messages":[{"r` + bs + `u006fle":"user","content":"escaped key"}]`,
	`"messages":[{"role":"user","role":"assistant","content":"repeated"}]`,
	`"messages":[],"messages":[{"role":"user","content":"repeated top-level"}]`,
	`"system":"a","SYSTEM":"b"`,
	`"messages":[{"role":"user","content":[{"type":"text","text":"a","text":"b"}]}]`,
	`"messages":[{"role":"user","content":[{"type":"text","text":5}]}]`,
	`"messages":[{"role":"user","content":[{"type":"text","text":"kept by system, skipped by user","tool_use_id":5}]}]`,
	`"messages":[{"role":"system","content":[{"type":"text","text":"kept by system, skipped by user","tool_use_id":5}]}]`,
	`"messages":[{"role":"user","content":["string block",null,[1]]}]`,
	`"messages":[{"role":"user","content":[null]}]`,
	`"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":"x"}]}]`,
	`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"u","file_id":5}}]}]`,
	`"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":7}]}]}]`,
	`"messages":[{"role":5,"content":"x"}]`,
	`"messages":[null]`,
	`"messages":[{"role":"user","content":{}}]`,
	`"messages":[{"role":"assistant","content":true}]`,
	`"messages":{}`,
	`"tools":{}`,
	`"tools":[{"name":5}]`,
	`"tools":[{"name":"f","cache_control":{"ttl":5}}]`,
	`"tools":["f"]`,
	`"system":[{"type":"text","text":"a"},"b"]`,
	`"model":5`,
	`"messages":[1,]`,
}

func flatTestBodies(t testing.TB) map[string][]byte {
	bodies := map[string][]byte{}
	for i, c := range flatAcceptCases {
		bodies[fmt.Sprintf("accept-%d", i)] = flatCaseBody(c)
	}
	for i, c := range flatDeclineCases {
		bodies[fmt.Sprintf("decline-%d", i)] = flatCaseBody(c)
	}
	for _, raw := range []string{`null`, `[]`, `"x"`, `{}`, `{"max_tokens":8}`, ` {"model":"m"} `, `{"model":"m"`} {
		bodies["top "+raw] = []byte(raw)
	}
	bodies["claude-code-2k"] = claudeCodeRequest(2 << 10)
	bodies["claude-code-64k"] = claudeCodeRequest(64 << 10)
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "*", "*", "request*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		bodies[p] = b
	}
	return bodies
}

// The flat decode must yield exactly the layered decode wherever it accepts a
// body, and must accept every real request shape the fixtures hold.
func TestParseRequestFlat_MatchesLayered(t *testing.T) {
	mustAccept := map[string]bool{"claude-code-2k": true, "claude-code-64k": true}
	for i := range flatAcceptCases {
		mustAccept[fmt.Sprintf("accept-%d", i)] = true
	}
	anthropicFixtures, _ := filepath.Glob(filepath.Join("..", "testdata", "anthropic", "*", "request.json"))
	for _, p := range anthropicFixtures {
		mustAccept[p] = true
	}
	for name, body := range flatTestBodies(t) {
		flat, ok := parseRequestFlat(body)
		layered, err := parseRequestLayered(body)
		switch {
		case ok && err != nil:
			t.Errorf("%s: flat accepted a body the layered decode rejects: %v", name, err)
		case ok && !reflect.DeepEqual(flat, layered):
			t.Errorf("%s: flat and layered differ:\nflat    %s\nlayered %s", name, mustJSON(flat), mustJSON(layered))
		case !ok && mustAccept[name]:
			t.Errorf("%s: flat declined a body it must decode", name)
		case ok && strings.HasPrefix(name, "decline-"):
			t.Errorf("%s: flat accepted a body it must leave to the layered decode", name)
		}
	}
}

func FuzzParseRequestFlat(f *testing.F) {
	for _, body := range flatTestBodies(f) {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		flat, ok := parseRequestFlat(body)
		if !ok {
			return
		}
		layered, err := parseRequestLayered(body)
		if err != nil {
			t.Fatalf("flat accepted a body the layered decode rejects: %v", err)
		}
		if !reflect.DeepEqual(flat, layered) {
			t.Fatalf("flat and layered differ:\nflat    %s\nlayered %s", mustJSON(flat), mustJSON(layered))
		}
	})
}

func BenchmarkParseRequest(b *testing.B) {
	for _, size := range []int{2 << 10, 400 << 10} {
		body := claudeCodeRequest(size)
		for _, dec := range []struct {
			name  string
			parse func([]byte) error
		}{
			{"layered", func(body []byte) error { _, err := parseRequestLayered(body); return err }},
			{"flat", func(body []byte) error { _, err := AnthropicTranslator{}.ParseRequest(body); return err }},
		} {
			b.Run(fmt.Sprintf("%dk/%s", size>>10, dec.name), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					if err := dec.parse(body); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// claudeCodeRequest builds a request of about size bytes. Under 8 KB it is
// one short turn; above, Claude Code's shape: a cached system array, 20 tools,
// and tool_use / tool_result turns carrying code that JSON must escape.
func claudeCodeRequest(size int) []byte {
	const code = "func handle(w http.ResponseWriter, r *http.Request) {\n\tbody, err := io.ReadAll(r.Body)\n\tif err != nil {\n\t\thttp.Error(w, \"read: \"+err.Error(), 400)\n\t\treturn\n\t}\n\tfmt.Fprintf(w, \"{\\\"ok\\\":true,\\\"n\\\":%d}\", len(body))\n}\n"
	prose := strings.Repeat("the router picks a binding, reserves the rate limit and selects a key. ", 4)
	req := map[string]any{
		"model": "claude-opus-4-1", "max_tokens": 32000, "stream": true,
		"metadata": map[string]any{"user_id": "user_" + strings.Repeat("a", 32)},
	}
	if size < 8<<10 {
		req["system"] = prose
		req["messages"] = []any{map[string]any{"role": "user", "content": strings.Repeat(prose, (size-700)/len(prose))}}
		return mustJSON(req)
	}
	ephemeral := map[string]any{"type": "ephemeral"}
	req["system"] = []any{
		map[string]any{"type": "text", "text": prose, "cache_control": ephemeral},
		map[string]any{"type": "text", "text": strings.Repeat(prose, 20) + strings.Repeat(code, 20), "cache_control": ephemeral},
	}
	tools := make([]any, 20)
	for i := range tools {
		schema := map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
			"path": map[string]any{"type": "string", "description": prose}, "content": map[string]any{"type": "string", "description": prose},
			"limit": map[string]any{"type": "integer"}, "replace_all": map[string]any{"type": "boolean", "default": false},
		}}
		tools[i] = map[string]any{"name": fmt.Sprintf("tool_%02d", i), "description": prose, "input_schema": schema}
	}
	req["tools"] = tools
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prose}}}}
	for i := 0; ; i++ {
		id := fmt.Sprintf("toolu_%04d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": prose},
				map[string]any{"type": "tool_use", "id": id, "name": fmt.Sprintf("tool_%02d", i%20), "input": map[string]any{"path": "/srv/app/handler.go", "content": strings.Repeat(code, 2+i%5)}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat(code, 6+i%20)},
			}},
		)
		req["messages"] = msgs
		if b := mustJSON(req); len(b) >= size {
			msgs[len(msgs)-1].(map[string]any)["content"] = append(msgs[len(msgs)-1].(map[string]any)["content"].([]any),
				map[string]any{"type": "text", "text": prose, "cache_control": ephemeral})
			return mustJSON(req)
		}
	}
}
