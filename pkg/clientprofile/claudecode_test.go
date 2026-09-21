package clientprofile

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func claudeCodeLister(t *testing.T) ModelLister {
	t.Helper()
	lister, ok := ClaudeCode().(ModelLister)
	if !ok {
		t.Fatal("ClaudeCode must implement ModelLister")
	}
	return lister
}

func TestClaudeCode_Identity(t *testing.T) {
	p := ClaudeCode()
	if p.Name() != "claude-code" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Shape() != "anthropic" {
		t.Errorf("Shape = %q", p.Shape())
	}
	if _, ok := p.(Router); !ok {
		t.Error("ClaudeCode must implement Router")
	}
	if _, ok := p.(Attributor); !ok {
		t.Error("ClaudeCode must implement Attributor")
	}
}

func TestClaudeCode_Match(t *testing.T) {
	cases := map[string]bool{
		// Both fixture User-Agents: inference and model discovery.
		"claude-cli/2.1.278 (external, sdk-cli)": true,
		"claude-code/2.1.278":                    true,
		"claude-cli/1.0":                         true,
		"curl/8.13.0":                            false,
		"Bun/1.4.3":                              false,
		"":                                       false,
	}
	for ua, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if got := ClaudeCode().Match(r); got != want {
			t.Errorf("Match(User-Agent=%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestClaudeCode_ModelsProjection(t *testing.T) {
	body, contentType, err := claudeCodeLister(t).Models([]ModelEntry{
		{
			ID:          "claude-sonnet-4-6",
			Model:       "claude-sonnet-4-6",
			DisplayName: "Claude Sonnet 4.6",
			Pointer:     true,
			Aliases:     []string{"sonnet"},
			Hosts: []ModelHost{
				{Name: "anthropic", Priced: true, InputUSDPerMtok: 3, OutputUSDPerMtok: 15},
				{Name: "bedrock"},
			},
		},
		{ID: "gpt-6-astra", Model: "gpt-6-astra", DisplayName: "GPT-6 Astra", Pointer: true},
		// One model, four snapshots: only the pointer row keeps the plain label.
		{ID: "deepseek-v4-flash", Model: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", Pointer: true, Aliases: []string{"flash"}},
		{ID: "deepseek-v4-flash-cloud", Model: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash"},
		{ID: "deepseek-v4-flash-0731", Model: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash"},
		{ID: "deepseek-v4-flash-free", Model: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash"},
	})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("contentType = %q", contentType)
	}

	const want = `{"data":[` +
		`{"type":"model","id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6","created_at":"1970-01-01T00:00:00Z","description":"anthropic · $3/$15 per Mtok, bedrock"},` +
		`{"type":"model","id":"claude/gpt-6-astra","display_name":"GPT-6 Astra","created_at":"1970-01-01T00:00:00Z"},` +
		`{"type":"model","id":"claude/deepseek-v4-flash","display_name":"DeepSeek V4 Flash","created_at":"1970-01-01T00:00:00Z"},` +
		`{"type":"model","id":"claude/deepseek-v4-flash-cloud","display_name":"DeepSeek V4 Flash (cloud)","created_at":"1970-01-01T00:00:00Z"},` +
		`{"type":"model","id":"claude/deepseek-v4-flash-0731","display_name":"DeepSeek V4 Flash (0731)","created_at":"1970-01-01T00:00:00Z"},` +
		`{"type":"model","id":"claude/deepseek-v4-flash-free","display_name":"DeepSeek V4 Flash (free)","created_at":"1970-01-01T00:00:00Z"}` +
		`],"has_more":false,"first_id":"claude-sonnet-4-6","last_id":"claude/deepseek-v4-flash-free"}`
	if string(body) != want {
		t.Errorf("Models body mismatch\n got: %s\nwant: %s", body, want)
	}
	// An alias is a spelling of a listed model, not a picker choice.
	for _, alias := range []string{"sonnet", "flash"} {
		if strings.Contains(string(body), `"`+pickerPrefix+alias+`"`) {
			t.Errorf("alias %q must not be listed", alias)
		}
	}
}

func TestClaudeCode_ModelsEmpty(t *testing.T) {
	body, _, err := claudeCodeLister(t).Models(nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if string(body) != `{"data":[],"has_more":false}` {
		t.Errorf("empty list = %s", body)
	}
	var probe struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("empty list must stay valid JSON: %v", err)
	}
}

func TestClaudeCode_Inbound(t *testing.T) {
	namer, ok := ClaudeCode().(ModelNamer)
	if !ok {
		t.Fatal("ClaudeCode must implement ModelNamer")
	}
	cases := map[string]string{
		"claude/gemma4-e4b":             "gemma4-e4b",
		"claude/gemma4-e4b[1m]":         "gemma4-e4b[1m]",
		"claude/gpt-6-astra@openrouter": "gpt-6-astra@openrouter",
		"claude-sonnet-4-6":             "claude-sonnet-4-6",
		"gemma4-e4b":                    "gemma4-e4b",
		"anthropic/claude-sonnet-4-6":   "anthropic/claude-sonnet-4-6",
		"":                              "",
	}
	for in, want := range cases {
		if got := namer.Inbound(in); got != want {
			t.Errorf("Inbound(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostsDescription(t *testing.T) {
	cases := []struct {
		name  string
		hosts []ModelHost
		want  string
	}{
		{name: "unpriced", hosts: []ModelHost{{Name: "ollama-self"}}, want: "ollama-self"},
		{
			name:  "priced trims trailing zeros",
			hosts: []ModelHost{{Name: "anthropic", Priced: true, InputUSDPerMtok: 3, OutputUSDPerMtok: 15}},
			want:  "anthropic · $3/$15 per Mtok",
		},
		{
			name:  "priced keeps two decimals",
			hosts: []ModelHost{{Name: "openai", Priced: true, InputUSDPerMtok: 1.25, OutputUSDPerMtok: 2.5}},
			want:  "openai · $1.25/$2.5 per Mtok",
		},
		{
			name:  "free meter is an explicit zero",
			hosts: []ModelHost{{Name: "local", Priced: true}},
			want:  "local · $0/$0 per Mtok",
		},
		{
			name: "multiple hosts",
			hosts: []ModelHost{
				{Name: "anthropic", Priced: true, InputUSDPerMtok: 3, OutputUSDPerMtok: 15},
				{Name: "bedrock", Priced: true, InputUSDPerMtok: 3.3, OutputUSDPerMtok: 16.5},
				{Name: "vertex"},
			},
			want: "anthropic · $3/$15 per Mtok, bedrock · $3.3/$16.5 per Mtok, vertex",
		},
		{name: "no hosts", hosts: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostsDescription(tc.hosts); got != tc.want {
				t.Errorf("hostsDescription = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeCode_HelloIsPublicAndEmpty(t *testing.T) {
	routes := ClaudeCode().(Router).Routes()
	if len(routes) != 2 {
		t.Fatalf("Routes len = %d, want 2", len(routes))
	}
	methods := map[string]bool{}
	for _, route := range routes {
		if route.Path != "/api/hello" {
			t.Errorf("unexpected route path %q", route.Path)
		}
		if !route.Public {
			t.Errorf("%s %s must be public — the client probes it before attaching credentials", route.Method, route.Path)
		}
		methods[route.Method] = true

		rec := httptest.NewRecorder()
		route.Handler.ServeHTTP(rec, httptest.NewRequest(route.Method, "/claude-code/api/hello", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /api/hello = %d, want 200", route.Method, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s /api/hello wrote a body: %q", route.Method, rec.Body.String())
		}
	}
	if !methods[http.MethodHead] || !methods[http.MethodGet] {
		t.Errorf("Routes methods = %v, want HEAD and GET", methods)
	}
}

func TestClaudeCode_AttributionHeadersAreLowercase(t *testing.T) {
	names := ClaudeCode().(Attributor).AttributionHeaders()
	if len(names) != 7 {
		t.Fatalf("AttributionHeaders len = %d, want 7", len(names))
	}
	for _, n := range names {
		if n != strings.ToLower(n) {
			t.Errorf("header name %q must be lowercase — it doubles as the metadata key", n)
		}
		if !strings.HasPrefix(n, "x-claude-code-") {
			t.Errorf("unexpected attribution header %q", n)
		}
	}
}

// TestClaudeCode_AgainstRecordedRequests pins the profile to what the real
// client sends; testdata/claude-code/README says how to refresh it.
func TestClaudeCode_AgainstRecordedRequests(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude-code/requests.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fixture := string(raw)

	for _, want := range []string{
		"=== HEAD /claude-code/api/hello",
		"=== GET /claude-code/v1/models?limit=1000",
		"=== POST /claude-code/v1/messages?beta=true",
	} {
		if !strings.Contains(fixture, want) {
			t.Errorf("fixture missing request line %q", want)
		}
	}

	// Both recorded User-Agents must resolve to the profile.
	for _, ua := range []string{"claude-cli/2.1.278 (external, sdk-cli)", "claude-code/2.1.278"} {
		if !strings.Contains(fixture, "User-Agent: "+ua) {
			t.Errorf("fixture missing User-Agent %q", ua)
		}
		r := httptest.NewRequest(http.MethodPost, "/claude-code/v1/messages", nil)
		r.Header.Set("User-Agent", ua)
		if !ClaudeCode().Match(r) {
			t.Errorf("Match must accept the recorded User-Agent %q", ua)
		}
	}

	// Attribution names must match the recorded headers case-insensitively;
	// net/http canonicalises on Get, the fixture keeps the wire casing.
	lower := strings.ToLower(fixture)
	for _, n := range []string{"x-claude-code-session-id", "x-claude-code-request-class"} {
		if !strings.Contains(lower, n+":") {
			t.Errorf("fixture missing attribution header %q", n)
		}
	}
}
