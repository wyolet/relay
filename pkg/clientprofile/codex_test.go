package clientprofile

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCodex_Identity(t *testing.T) {
	p := Codex()
	if p.Name() != "codex" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Shape() != "openai_responses" {
		t.Errorf("Shape = %q", p.Shape())
	}
	if _, ok := p.(Attributor); !ok {
		t.Error("Codex must implement Attributor")
	}
	if _, ok := p.(SessionKeyer); !ok {
		t.Error("Codex must implement SessionKeyer")
	}
	if _, ok := p.(ModelLister); !ok {
		t.Error("Codex must implement ModelLister")
	}
	// The client probes no endpoint and needs no id reshaping.
	if _, ok := p.(Router); ok {
		t.Error("Codex must not implement Router")
	}
	if _, ok := p.(ModelNamer); ok {
		t.Error("Codex must not implement ModelNamer")
	}
	if _, ok := p.(TokenCountRoute); ok {
		t.Error("Codex must not implement TokenCountRoute")
	}
}

// The document is Codex's own /models shape, saved to disk and handed back
// through model_catalog_json; the field names are pinned against
// codex-rs/protocol/src/openai_models.rs at tag rust-v0.153.4, so the golden
// is exact rather than a round-trip.
func TestCodex_ModelsProjection(t *testing.T) {
	lister, ok := Codex().(ModelLister)
	if !ok {
		t.Fatal("Codex must implement ModelLister")
	}
	body, contentType, err := lister.Models([]ModelEntry{
		{
			ID:            "deepseek-v4-1-flash-cloud",
			Model:         "deepseek-v4-1-flash",
			DisplayName:   "DeepSeek V4.1 Flash",
			Pointer:       true,
			ContextWindow: 1000000,
			Reasoning:     true,
			ToolCall:      true,
			Hosts: []ModelHost{
				{Name: "deepseek", Priced: true, InputUSDPerMtok: 0.28, OutputUSDPerMtok: 0.42},
				{Name: "ollama-self"},
			},
		},
		// No window and no reasoning: the fields go out empty rather than guessed.
		{ID: "gemma4-e4b-0731", Model: "gemma4-e4b", DisplayName: "Gemma 4 E4B"},
	})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("contentType = %q", contentType)
	}

	const want = `{"models":[` +
		`{"slug":"deepseek-v4-1-flash-cloud","display_name":"DeepSeek V4.1 Flash","description":"deepseek · $0.28/$0.42 per Mtok, ollama-self",` +
		`"base_instructions":"","supported_reasoning_levels":[` +
		`{"effort":"low","description":"Faster answers, less reasoning"},` +
		`{"effort":"medium","description":"Balanced reasoning"},` +
		`{"effort":"high","description":"Slower answers, more reasoning"}],` +
		`"shell_type":"unified_exec","visibility":"list","supported_in_api":true,"priority":0,` +
		`"support_verbosity":false,"truncation_policy":{"mode":"bytes","limit":10000},"context_window":1000000,` +
		`"experimental_supported_tools":[],"availability_nux":null,"upgrade":null,"default_verbosity":null,"apply_patch_tool_type":null},` +
		`{"slug":"gemma4-e4b-0731","display_name":"Gemma 4 E4B (0731)","description":"",` +
		`"base_instructions":"","supported_reasoning_levels":[],` +
		`"shell_type":"unified_exec","visibility":"list","supported_in_api":true,"priority":1,` +
		`"support_verbosity":false,"truncation_policy":{"mode":"bytes","limit":10000},` +
		`"experimental_supported_tools":[],"availability_nux":null,"upgrade":null,"default_verbosity":null,"apply_patch_tool_type":null}` +
		`]}`
	if string(body) != want {
		t.Errorf("Models body mismatch\n got: %s\nwant: %s", body, want)
	}
}

// Codex rejects a catalog file with no models, so an empty listing stays a
// valid document the operator can see is empty rather than a broken one.
func TestCodex_ModelsEmpty(t *testing.T) {
	body, _, err := Codex().(ModelLister).Models(nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if string(body) != `{"models":[]}` {
		t.Errorf("empty catalog = %s", body)
	}
}

func TestCodex_Match(t *testing.T) {
	cases := map[string]bool{
		"codex_exec/0.153.4 (Mac OS 27.0.0; arm64) ghostty/1.3.1 (codex_exec; 0.153.4)": true,
		"codex_cli_rs/0.153.4 (Mac OS 27.0.0; arm64)":                                   true,
		"codex_tui/0.153.4":                      true,
		"codex_mcp_server/0.153.4":               true,
		"claude-cli/2.1.278 (external, sdk-cli)": false,
		"curl/8.13.0":                            false,
		"":                                       false,
	}
	for ua, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if got := Codex().Match(r); got != want {
			t.Errorf("Match(User-Agent=%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestCodex_AttributionHeaders(t *testing.T) {
	names := Codex().(Attributor).AttributionHeaders()
	for _, n := range names {
		if n != strings.ToLower(n) {
			t.Errorf("header name %q must be lowercase — it doubles as the metadata key", n)
		}
	}
	// The two turn blobs are opaque and large; recording them on every usage
	// event is the thing this list exists to avoid.
	for _, n := range names {
		if n == "x-codex-turn-metadata" || n == "x-codex-turn-state" {
			t.Errorf("%q must not be recorded", n)
		}
	}
}

func TestCodex_SessionKey(t *testing.T) {
	keyer := Codex().(SessionKeyer)
	h := http.Header{}
	if got := keyer.SessionKey(h); got != "" {
		t.Errorf("SessionKey without the header = %q, want empty", got)
	}
	h.Set("Session-Id", "01a0c1d8-3348-7993-ad4c-a8d342ba89c8")
	if got := keyer.SessionKey(h); got != "01a0c1d8-3348-7993-ad4c-a8d342ba89c8" {
		t.Errorf("SessionKey = %q", got)
	}
}

// TestCodex_AgainstRecordedRequests pins the profile to what the real client
// sends; testdata/codex/README says how to refresh it.
func TestCodex_AgainstRecordedRequests(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex/requests.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fixture := string(raw)

	if !strings.Contains(fixture, "=== POST /codex/v1/responses") {
		t.Error("fixture missing the inference request line")
	}

	// The recorded User-Agent must resolve to the profile.
	const ua = "codex_exec/0.153.4 (Mac OS 27.0.0; arm64) ghostty/1.3.1 (codex_exec; 0.153.4)"
	if !strings.Contains(fixture, "user-agent: "+ua) {
		t.Errorf("fixture missing User-Agent %q", ua)
	}
	r := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", nil)
	r.Header.Set("User-Agent", ua)
	if !Codex().Match(r) {
		t.Errorf("Match must accept the recorded User-Agent %q", ua)
	}

	// Every attribution header the fixture recorded must be on the list; the
	// rest only ship on sub-agent and resumed turns (see the README).
	lower := strings.ToLower(fixture)
	recorded := map[string]bool{}
	for _, n := range codexAttributionHeaders {
		recorded[n] = true
	}
	for _, n := range []string{"session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "originator"} {
		if !strings.Contains(lower, "\n"+n+":") {
			t.Errorf("fixture missing header %q", n)
		}
		if !recorded[n] {
			t.Errorf("header %q is in the fixture but not recorded as attribution", n)
		}
	}
}
