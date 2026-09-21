package clientprofile

import (
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

// No ModelLister: Codex only refreshes models for a ChatGPT-backend or command-auth provider, so a relay provider configured with env_key is never asked for a list, and the endpoint it would call answers {"models":[…]} — a shape we have not pinned against the client. The forward path for handing Codex relay's catalog is its model_catalog_json config key, which replaces the bundled catalog wholesale; pinning that document's schema is a separate piece of work.
