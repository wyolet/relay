// The HTTP side of the pseudo kinds (app/pseudo): which kinds RELAY_PSEUDO_KINDS turns on, and the answer shapes and documented responses they share. Running a request is in pseudo_run.go, answering a refusal in pseudo_errors.go; each kind's route lives in its own pseudo_<kind>.go.

package control

import (
	"log/slog"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/meta"
)

// pseudoKinds registers each pseudo kind's route, by the name RELAY_PSEUDO_KINDS turns it on with.
var pseudoKinds = map[string]func(huma.API, Deps, huma.Middlewares){
	"agents":         registerPseudoAgent,
	"access":         registerPseudoAccess,
	"limits":         registerPseudoLimits,
	"host-keys":      registerPseudoHostKey,
	"agent-removals": registerPseudoAgentRemoval,
}

// pseudoKindsAll in RELAY_PSEUDO_KINDS turns every pseudo kind on.
const pseudoKindsAll = "all"

// enabledPseudoKinds is the pseudo kinds names turns on, sorted; a name no kind has is left out.
func enabledPseudoKinds(names []string) []string {
	out := []string{}
	for kind := range pseudoKinds {
		if slices.Contains(names, kind) || slices.Contains(names, pseudoKindsAll) {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}

// registerPseudoKinds registers the route of each pseudo kind d turns on; the others have no route at all.
func registerPseudoKinds(api huma.API, d Deps, protect huma.Middlewares) {
	for _, name := range d.PseudoKinds {
		if _, ok := pseudoKinds[name]; !ok && name != pseudoKindsAll {
			slog.Warn("relay control: RELAY_PSEUDO_KINDS names no pseudo kind; ignored", "name", name)
		}
	}
	for _, kind := range enabledPseudoKinds(d.PseudoKinds) {
		pseudoKinds[kind](api, d, protect)
	}
}

// CreatedRow is one real row a pseudo object made, or would make on a dry run.
type CreatedRow struct {
	Kind string `json:"kind" doc:"API kind of the row, such as service-account or policy-binding."`
	ID   string `json:"id"   doc:"Id of the row; minted before the write, so a dry run answers it too."`
	Name string `json:"name" doc:"Slug of the row."`
}

// RowRef names one row.
type RowRef struct {
	ID   string `json:"id"   doc:"Id of the row."`
	Name string `json:"name" doc:"Slug of the row."`
}

func rowRef(m *meta.Metadata) RowRef { return RowRef{ID: m.ID, Name: m.Name} }

func createdRows(rows []pseudo.Row) []CreatedRow { return stepRows(rows, pseudo.OpCreate) }

// PseudoIssue is one plan issue a pseudo object's rows would raise.
type PseudoIssue struct {
	Code    string `json:"code"              doc:"Stable snake_case diagnostic identifier, as GET /plan-issues answers it."`
	Message string `json:"message"           doc:"What would break."`
	Kind    string `json:"kind"              doc:"Kind of the object that would carry the issue."`
	ID      string `json:"id,omitempty"      doc:"Id of that object; empty when the caller may not see it."`
	Name    string `json:"name,omitempty"    doc:"Slug of that object; empty when the caller may not see it or it has none."`
	Field   string `json:"field,omitempty"   doc:"Spec path of the object the issue is about."`
	Pointer string `json:"pointer,omitempty" doc:"JSON pointer of the part of the body the object came from; empty for an object that exists already."`
}

// pseudoRefusal is a refused pseudo object: the usual error envelope, plus where in the body the refused part is and, for plan_issue, the issues.
type pseudoRefusal struct {
	httpapi.APIError
	Pointer  string         `json:"pointer,omitempty" doc:"JSON pointer of the part of the body the refused row came from, such as /spec/access/accountIds/1."`
	Issues   []PseudoIssue  `json:"issues,omitempty"  doc:"With plan_issue: each error the rows would raise in the plans that they do not have now."`
	Blockers []blockerGroup `json:"blockers,omitempty" doc:"With resource_in_use: the rows that still reference the row a delete step refused on, grouped as its own delete call groups them."`
}

// codePlanIssue refuses rows that pass one by one but would raise a plan error together.
const codePlanIssue = "plan_issue"

// pseudoResponses are the answers every pseudo route documents beyond its errors list.
func pseudoResponses(api huma.API, conflicts string) map[string]*huma.Response {
	return map[string]*huma.Response{
		"409": jsonResponse(api, conflicts, pseudoRefusal{}),
		"503": jsonResponse(api, "The plans are not loaded yet, or checking the rows against them took longer than RELAY_PLAN_CHECK_TIMEOUT_MS. Nothing was written.", pseudoRefusal{}),
	}
}

// documentPlanIssue documents the answers of the pseudo route at path once it is registered, over what huma writes for its errors list: every refusal carries the pseudo refusal body, and a dry run answers 200 with the body a write answers 201 with.
func documentPlanIssue(api huma.API, path string) {
	item := api.OpenAPI().Paths[path]
	if item == nil || item.Post == nil {
		return
	}
	responses := item.Post.Responses
	for code, desc := range map[string]string{
		"400": "The body, or a row it makes, breaks a rule the schema cannot express; error.message says which and pointer names the part of the body.",
		"403": "A row's own grant refuses it, as its own create call would; pointer names the part of the body it came from.",
		"404": "The body names a row the caller may not see; pointer names where.",
		"422": "plan_issue: every row passes its own checks, but together they would raise errors in the plans that are not there now, or a lane they make or bind would refuse every request; issues lists them, each with the pointer of the part of the body it came from. Nothing was written. Also a body that does not match the schema, with error.details.",
		"500": "The write failed on the server; nothing was written.",
	} {
		responses[code] = jsonResponse(api, desc, pseudoRefusal{})
	}
	if made := responses["201"]; made != nil {
		dry := *made
		dry.Description = "dryRun=true: what the write would make, ids included and key plaintexts left out. Nothing was written."
		responses["200"] = &dry
	}
}
