package clientprofile

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Codex builds every request URL by concatenating its configured base_url with "/responses" — it never inserts a "/v1" of its own — so pointing base_url at the /codex prefix is what lands a turn on the relay's Responses route.
//
// Verified against codex-cli 0.153.4; the recorded requests are in testdata/codex/.
type codex struct{}

// Codex returns the profile for OpenAI's Codex CLI.
func Codex() Profile { return codex{} }

func (codex) Name() string  { return "codex" }
func (codex) Shape() string { return "openai_responses" }

// uaPrefix covers every Codex surface: the User-Agent is "<originator>/<version> (…)" and each originator is codex_<surface> (codex_cli_rs, codex_exec, codex_tui, codex_mcp_server, codex_vscode), so the shared prefix matches all of them without pinning the list.
const uaPrefix = "codex_"

func (codex) Match(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("User-Agent"), uaPrefix)
}

// codexAttributionHeaders are the client's own request markers, each a short identifier worth keeping against a usage record.
//
// x-codex-turn-metadata and x-codex-turn-state are deliberately left out: the first is a multi-hundred-byte JSON blob repeated on every request (its useful ids — session, thread — are already captured from their own headers), and the second is an opaque resume token whose contents relay cannot interpret.
var codexAttributionHeaders = []string{
	"session-id",
	"thread-id",
	"x-client-request-id",
	"x-codex-installation-id",
	"x-codex-window-id",
	"x-codex-parent-thread-id",
	"x-openai-subagent",
	"originator",
}

func (codex) AttributionHeaders() []string { return codexAttributionHeaders }

// SessionKey reads the per-conversation marker — codexAttributionHeaders[0], named once so the two uses cannot drift apart. Codex asks no gateway to count tokens today, so nothing reads this yet; it is what a calibrated count would key on if it ever does.
func (codex) SessionKey(h http.Header) string { return h.Get(codexAttributionHeaders[0]) }

// Codex never asks a key-authenticated gateway for a model list — it refreshes only for a ChatGPT-backend or command-auth provider — so this document is served for the operator to save and hand back through the `model_catalog_json` config key, which replaces the bundled catalog wholesale. Its shape is Codex's own /models response, `{"models":[…]}`, not the OpenAI list-models envelope.
//
// Field set pinned against codex-rs/protocol/src/openai_models.rs (ModelInfo, ModelsResponse) at tag rust-v0.153.4. Unknown keys are ignored on read, but every key below must be present: Option-typed fields carrying no serde default are required and accept an explicit null.
type codexModelCatalog struct {
	Models []codexModelInfo `json:"models"`
}

type codexModelInfo struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Codex's deserializer rejects a model that carries neither `base_instructions` nor `model_messages.instructions_template`: the catalog owns the agent prompt, and a replacement catalog replaces it. Relay has no agent prompt of its own, so it sends the key empty and the docs point operators at `model_instructions_file` for the prompt they want.
	BaseInstructions         string                 `json:"base_instructions"`
	SupportedReasoningLevels []codexReasoningPreset `json:"supported_reasoning_levels"`
	ShellType                string                 `json:"shell_type"`
	Visibility               string                 `json:"visibility"`
	SupportedInAPI           bool                   `json:"supported_in_api"`
	Priority                 int                    `json:"priority"`
	SupportVerbosity         bool                   `json:"support_verbosity"`
	TruncationPolicy         codexTruncationPolicy  `json:"truncation_policy"`
	// ContextWindow is omitted when the catalog declares none; `max_context_window` is left out entirely so an operator's `model_context_window` is not clamped against a number relay invented.
	ContextWindow              int      `json:"context_window,omitempty"`
	ExperimentalSupportedTools []string `json:"experimental_supported_tools"`
	// Required keys relay holds no value for. Null is what Codex's own fallback metadata carries for each.
	AvailabilityNux    any `json:"availability_nux"`
	Upgrade            any `json:"upgrade"`
	DefaultVerbosity   any `json:"default_verbosity"`
	ApplyPatchToolType any `json:"apply_patch_tool_type"`
}

type codexReasoningPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

// Values Codex synthesises for a model it does not recognise (model_info_from_slug in codex-rs/models-manager/src/model_info.rs). Repeating them keeps a projected entry behaving exactly like the unrecognised slug it replaces, minus the metadata relay can actually fill.
const (
	codexShellType             = "unified_exec"
	codexTruncationMode        = "bytes"
	codexTruncationLimit int64 = 10_000
)

// codexPickerVisibility is the visibility that puts a row in the /model picker; Codex maps it to show_in_picker and sorts the list by ascending priority.
const codexPickerVisibility = "list"

// codexReasoningLevels lists the effort options the picker offers for a reasoning model. The three are relay-canonical (sdk/v1 ReasoningConfig.Effort), so they translate to whatever the routed upstream speaks; a model the catalog does not mark as reasoning offers none.
func codexReasoningLevels(reasoning bool) []codexReasoningPreset {
	if !reasoning {
		return []codexReasoningPreset{}
	}
	return []codexReasoningPreset{
		{Effort: "low", Description: "Faster answers, less reasoning"},
		{Effort: "medium", Description: "Balanced reasoning"},
		{Effort: "high", Description: "Slower answers, more reasoning"},
	}
}

// Models renders the catalog document, one entry per listed snapshot, ordered as relay listed them — priority is the index because Codex sorts on it. MaxOutputTokens and ToolCall are not projected: ModelInfo has no field for either.
func (codex) Models(entries []ModelEntry) ([]byte, string, error) {
	out := codexModelCatalog{Models: make([]codexModelInfo, 0, len(entries))}
	for i, e := range entries {
		out.Models = append(out.Models, codexModelInfo{
			Slug:                       e.ID,
			DisplayName:                entryLabel(e),
			Description:                hostsDescription(e.Hosts),
			SupportedReasoningLevels:   codexReasoningLevels(e.Reasoning),
			ShellType:                  codexShellType,
			Visibility:                 codexPickerVisibility,
			SupportedInAPI:             true,
			Priority:                   i,
			TruncationPolicy:           codexTruncationPolicy{Mode: codexTruncationMode, Limit: codexTruncationLimit},
			ContextWindow:              e.ContextWindow,
			ExperimentalSupportedTools: []string{},
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	return body, "application/json", nil
}
