package clientprofile

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Claude Code resolves every gateway path against ANTHROPIC_BASE_URL, so
// the profile prefix has to carry model discovery and the startup probe
// alongside the messages endpoint, not just the inference route.
//
// Contract: https://code.claude.com/docs/en/llm-gateway-protocol, verified
// against claude-cli 2.1.278 — the recorded requests are in
// testdata/claude-code/.
type claudeCode struct{}

// ClaudeCode returns the profile for Anthropic's Claude Code CLI.
func ClaudeCode() Profile { return claudeCode{} }

func (claudeCode) Name() string  { return "claude-code" }
func (claudeCode) Shape() string { return "anthropic" }

// Match accepts both User-Agent families the client uses: inference posts
// as claude-cli/, model discovery as claude-code/.
func (claudeCode) Match(r *http.Request) bool {
	ua := r.Header.Get("User-Agent")
	return strings.HasPrefix(ua, "claude-cli/") || strings.HasPrefix(ua, "claude-code/")
}

// modelCreatedAt is a placeholder: the client reads only id, display_name
// and description out of the discovery response.
const modelCreatedAt = "1970-01-01T00:00:00Z"

type anthropicModel struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
	Description string `json:"description,omitempty"`
}

type anthropicModelList struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID string           `json:"first_id,omitempty"`
	LastID  string           `json:"last_id,omitempty"`
}

// pickerPrefix is the shape the client's own filter forces: Claude Code keeps a discovered model only when its id contains "claude" or "anthropic" (case-insensitive), so every other id is listed as claude/<id>. The "/" reads as provider/model in the picker the way "vertex_ai/claude-…" does, and it fails closed if a minted id ever reaches routing unstripped — the catalog reference grammar (app/modelref) reads "claude/gemma4-e4b" as provider "claude", which no catalog defines, so the request ends in model-not-found rather than mis-routed.
const pickerPrefix = "claude/"

// pickerID mints the id the client's picker will keep. Ids already carrying the substring go out exactly as the catalog names them, so nothing is listed twice.
func pickerID(id string) string {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic") {
		return id
	}
	return pickerPrefix + id
}

// Inbound strips the minted prefix, so any suffix the caller appended (an alias bracket, a host pin) rides through untouched.
func (claudeCode) Inbound(model string) string { return strings.TrimPrefix(model, pickerPrefix) }

// entryLabel names the row in the picker, which shows display_name and description and nothing else: every snapshot of one model carries the parent's display name, so the non-pointer ones would render as identical rows without the snapshot suffix appended here.
func entryLabel(e ModelEntry) string {
	if e.Pointer {
		return e.DisplayName
	}
	suffix := e.ID
	switch {
	case e.ID == e.Model:
		suffix = "base"
	case e.Model != "":
		suffix = strings.TrimPrefix(e.ID, e.Model+"-")
	}
	return e.DisplayName + " (" + suffix + ")"
}

// Models renders the Anthropic list-models document, one row per entry, each id minted into the form the client's picker keeps. Aliases are left out: an alias is a compatibility spelling of a model already listed, not a separate choice, and it still resolves when the client sends it.
func (claudeCode) Models(entries []ModelEntry) ([]byte, string, error) {
	out := anthropicModelList{Data: make([]anthropicModel, 0, len(entries))}
	for _, e := range entries {
		out.Data = append(out.Data, anthropicModel{
			Type:        "model",
			ID:          pickerID(e.ID),
			DisplayName: entryLabel(e),
			CreatedAt:   modelCreatedAt,
			Description: hostsDescription(e.Hosts),
		})
	}
	if n := len(out.Data); n > 0 {
		out.FirstID = out.Data[0].ID
		out.LastID = out.Data[n-1].ID
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return body, "application/json", nil
}

// hostsDescription renders the picker subtitle: every host serving the
// entry, with its rates when the binding is priced.
func hostsDescription(hosts []ModelHost) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !h.Priced {
			parts = append(parts, h.Name)
			continue
		}
		parts = append(parts, h.Name+" · $"+usdRate(h.InputUSDPerMtok)+"/$"+usdRate(h.OutputUSDPerMtok)+" per Mtok")
	}
	return strings.Join(parts, ", ")
}

// usdRate formats a per-Mtok amount with at most two decimals and no
// trailing zeros.
func usdRate(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// Routes exposes the connection-warming probe. The client sends it before
// it has necessarily attached credentials, so it is public, and it answers
// with a bare 200 — nothing about the deployment leaks.
func (claudeCode) Routes() []Route {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return []Route{
		{Method: http.MethodHead, Path: "/api/hello", Handler: hello, Public: true},
		{Method: http.MethodGet, Path: "/api/hello", Handler: hello, Public: true},
	}
}

// attributionHeaders are the client's own request markers. The last four
// only ship when the client sets CLAUDE_CODE_GATEWAY_HINT_HEADERS=1.
var attributionHeaders = []string{
	"x-claude-code-session-id",
	"x-claude-code-agent-id",
	"x-claude-code-parent-agent-id",
	"x-claude-code-request-class",
	"x-claude-code-agent-type",
	"x-claude-code-compaction",
	"x-claude-code-context-compacted",
}

func (claudeCode) AttributionHeaders() []string { return attributionHeaders }
