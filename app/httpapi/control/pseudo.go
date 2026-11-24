// The HTTP side of the pseudo kinds (app/pseudo): which kinds RELAY_PSEUDO_KINDS turns on, the create each real row kind goes through, and how a refused pseudo object is answered. Each kind's route lives in its own pseudo_<kind>.go.

package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
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

// pseudoNamer mints names against the rows in d's stores, listing each kind's names once.
func (d Deps) pseudoNamer() *pseudo.Namer {
	taken := map[string]func(string) bool{}
	return &pseudo.Namer{Taken: func(kind, name string) bool {
		f, ok := taken[kind]
		if !ok {
			f = d.namesTaken(kind)
			taken[kind] = f
		}
		return f(name)
	}}
}

// namesTaken reports whether a stored row of the API kind holds a name.
func (d Deps) namesTaken(kind string) func(string) bool {
	s := d.Stores
	switch kind {
	case "service-account":
		return slugTakenFn(s.ServiceAccount, func(r *serviceaccount.ServiceAccount) *meta.Metadata { return &r.Meta })
	case "account":
		return slugTakenFn(s.Account, func(r *account.Account) *meta.Metadata { return &r.Meta })
	case "pool":
		return slugTakenFn(s.Pool, func(r *pool.Pool) *meta.Metadata { return &r.Meta })
	case "policy":
		return slugTakenFn(s.Policy, func(r *policy.Policy) *meta.Metadata { return &r.Meta })
	case "policy-binding":
		return slugTakenFn(s.PolicyBinding, func(r *policybinding.PolicyBinding) *meta.Metadata { return &r.Meta })
	case "key":
		return slugTakenFn(s.Key, func(r *key.Key) *meta.Metadata { return &r.Meta })
	case "host-key":
		return slugTakenFn(s.HostKey, func(r *hostkey.HostKey) *meta.Metadata { return &r.Meta })
	}
	return func(string) bool { return false }
}

// recordUpstream records, for the plan check, what serves through the accounts and host keys named: each account, the real account behind a share, and every host key of those accounts. A kind calls it for the accounts and keys its rows draw on, so keys written a moment ago count.
func (d Deps) recordUpstream(ctx context.Context, accountIDs, credentialIDs []string) error {
	var held []string
	for _, id := range accountIDs {
		a, err := d.Stores.Account.Get(ctx, id)
		if err != nil {
			return err
		}
		if a == nil {
			continue
		}
		pseudo.Record(ctx, id, a)
		held = append(held, id)
		if src := a.Spec.SourceAccountID; src != "" {
			s, err := d.Stores.Account.Get(ctx, src)
			if err != nil {
				return err
			}
			pseudo.Record(ctx, src, s)
			held = append(held, src)
		}
	}
	keys, err := d.Stores.HostKey.IDsInAccounts(ctx, held)
	if err != nil {
		return err
	}
	for _, id := range append(keys, credentialIDs...) {
		k, err := d.Stores.HostKey.Get(ctx, id)
		if err != nil {
			return err
		}
		pseudo.Record(ctx, id, k)
	}
	return nil
}

// pseudoWrite is one pseudo object's write: it creates each row as the row's own create route would, and keeps the audit rows and key plaintexts of what it made.
type pseudoWrite struct {
	d          Deps
	audit      []audit.Row
	plaintexts map[string]string
	// warnings are the new plan warnings on the rows made, as a kind's answer lists them ([] for none).
	warnings []PseudoIssue
}

// runPseudo writes the rows expand returns in one transaction (pseudo.Writer), after the outside writes; a dry run writes nothing and records no audit row.
func (d Deps) runPseudo(ctx context.Context, expand pseudo.ExpandFunc, dryRun bool, outside ...pseudo.Outside) (*pseudoWrite, []pseudo.Row, error) {
	w := &pseudoWrite{d: d, plaintexts: map[string]string{}}
	writer := pseudo.Writer{Stores: d.Stores, Check: d.PlanCheck, View: d.PlanView, Outside: outside}
	rows, warnings, err := writer.RunWithWarnings(ctx, expand, w.write, dryRun)
	if err != nil {
		err = d.pseudoError(ctx, err)
	} else {
		w.warnings = d.pseudoIssues(ctx, rows, warnings)
	}
	switch {
	case dryRun:
		audit.Discard(ctx)
	case err != nil:
		audit.RecordEach(ctx, []audit.Row{refusedRow(ctx, err)})
	default:
		audit.RecordEach(ctx, w.audit)
	}
	if err != nil {
		return nil, nil, err
	}
	return w, rows, nil
}

// refusedRow is the one audit row of a refused pseudo request: it names the request (pseudo-agents.create, from the route) rather than the row it got to, since nothing was written; a refused grant is denied, any other refusal an error.
func refusedRow(ctx context.Context, refused error) audit.Row {
	code := http.StatusInternalServerError
	var se huma.StatusError
	if errors.As(refused, &se) {
		code = se.GetStatus()
	}
	plural := "pseudo"
	if rc := chi.RouteContext(ctx); rc != nil && rc.RoutePattern() != "" {
		plural = path.Base(rc.RoutePattern())
	}
	status := audit.StatusError
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		status = audit.StatusDenied
	}
	return audit.Row{Action: plural + ".create", Resource: audit.Resource{Kind: strings.TrimSuffix(plural, "s")}, Status: status, Code: code}
}

func (w *pseudoWrite) create(ctx context.Context, tx *appcatalog.Stores, r pseudo.Row) error {
	d := w.d.boundTo(tx)
	var err error
	switch v := r.Value.(type) {
	case *serviceaccount.ServiceAccount:
		err = d.createPseudoServiceAccount(ctx, v)
	case *account.Account:
		err = d.createPseudoAccount(ctx, v)
	case *pool.Pool:
		if err = checkCreate(ctx, poolChecks(d), "", &v.Meta, v); err == nil {
			err = insertRow(ctx, d.Stores.Pool, v)
		}
	case *policy.Policy:
		if err = checkCreate(ctx, policyChecks(d), "", &v.Meta, v); err == nil {
			err = insertRow(ctx, d.Stores.Policy, v)
		}
	case *pseudo.PolicyCopy:
		if err = checkCreate(ctx, policyChecksCopying(d, refcheck.PolicyLinks(v.Source)), "", &v.Policy.Meta, v.Policy); err == nil {
			err = insertRow(ctx, d.Stores.Policy, v.Policy)
		}
	case *policybinding.PolicyBinding:
		_, _, err = d.createBinding(ctx, v)
	case *budget.Budget:
		if err = checkCreate(ctx, budgetChecks(d), "", &v.Meta, v); err == nil {
			err = insertRow(ctx, d.Stores.Budget, v)
		}
	case *ratelimit.RateLimit:
		if err = checkCreate(ctx, rateLimitChecks(d), "", &v.Meta, v); err == nil {
			err = insertRow(ctx, d.Stores.RateLimit, v)
		}
	case *key.Key:
		var plaintext string
		plaintext, err = d.createKey(ctx, v, createKeySpec{AccountID: v.Spec.AccountID, ExpiresAt: v.Spec.ExpiresAt, Enabled: v.Spec.Enabled, PassthroughAllowed: v.Spec.PassthroughAllowed, PayloadLoggingEnabled: v.Spec.PayloadLoggingEnabled})
		w.plaintexts[v.Meta.ID] = plaintext
	case *hostkey.HostKey:
		err = d.createPseudoHostKey(ctx, v)
	case *pseudo.AccountRebinding:
		return w.rebindAccount(ctx, d, v)
	default:
		err = fmt.Errorf("no create for a %T row", r.Value)
	}
	if err != nil {
		return err
	}
	w.audit = append(w.audit, audit.Row{Action: authz.Plural(r.Kind) + ".create", Resource: d.auditResource(r.Kind, r.Meta.ID, r.Meta.Name, r.Meta.Owner), Fields: []string{audit.AnyField}})
	return nil
}

// createPseudoServiceAccount runs the checks POST /service-accounts runs on sa, which has its id and name, and writes it through the directory.
func (d Deps) createPseudoServiceAccount(ctx context.Context, sa *serviceaccount.ServiceAccount) error {
	if err := checkCreate(ctx, serviceAccountChecks(d), "", &sa.Meta, sa); err != nil {
		return err
	}
	return insertRow(ctx, newDirectoryKinds(d, newDirectoryOps(d)).serviceAccounts, sa)
}

// createPseudoAccount writes a share with the checks POST /accounts/{ref}/shares runs, or a caller account as relay keeps one.
func (d Deps) createPseudoAccount(ctx context.Context, a *account.Account) error {
	switch {
	case a.IsShared():
		return d.createPseudoShare(ctx, a)
	case a.ManagedKey() != "":
		return d.createPseudoManaged(ctx, a)
	case a.IsProvider():
		return d.createPseudoProviderAccount(ctx, a)
	}
	return fmt.Errorf("account %q: only a share, a caller account or a provider account is made here", a.Meta.Name)
}

func (d Deps) createPseudoShare(ctx context.Context, share *account.Account) error {
	src, err := d.Stores.Account.Get(ctx, share.Spec.SourceAccountID)
	if err != nil {
		return err
	}
	if src == nil || !visibleTo(ctx, d.Authz, "account", src.Meta.ID, src.Meta.Owner) {
		return huma.Error404NotFound(fmt.Sprintf("account %q not found", share.Spec.SourceAccountID))
	}
	pseudo.Record(ctx, src.Meta.ID, src)
	if !src.IsProvider() {
		return huma.Error400BadRequest(fmt.Sprintf("account %q is a %s account; only a provider account is shared", src.Meta.Name, src.Spec.Kind))
	}
	c := refs(d)
	if err := share.Validate(); err != nil {
		return huma.Error400BadRequest(err.Error())
	}
	if err := c.Share(ctx, src, share); err != nil {
		return refErr(err)
	}
	// Worded before the write: once it fails the transaction is aborted and reads nothing.
	taken := refErr(c.ShareExists(ctx, src, share.Receiver()))
	if err := d.Stores.Account.Upsert(ctx, share); err != nil {
		if errors.Is(err, account.ErrShareTaken) {
			return taken
		}
		return err
	}
	return nil
}

// createPseudoManaged writes a caller account under the id it was given, as relay's account writer does for a new team, project, user or service account.
func (d Deps) createPseudoManaged(ctx context.Context, a *account.Account) error {
	if err := a.Validate(); err != nil {
		return huma.Error400BadRequest(err.Error())
	}
	id, err := d.Stores.Account.EnsureManaged(ctx, a)
	if err != nil {
		return err
	}
	if id != a.Meta.ID {
		return conflictError(fmt.Sprintf("account %q was made by another change at the same time; nothing was written, retry the request", a.Meta.Name))
	}
	return nil
}

func conflictError(msg string) error {
	return &httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "conflict", Message: msg}, HTTPStatus: http.StatusConflict}
}

// pseudoError answers a pseudo write that wrote nothing.
func (d Deps) pseudoError(ctx context.Context, err error) error {
	var be *pseudo.BodyError
	var ie *pseudo.IssuesError
	var re *pseudo.RowError
	var answered *pseudoRefusal
	var ue *pseudo.UndoError
	var oe *pseudo.OrganizationError
	var changed *removalChanged
	switch {
	case errors.As(err, &answered):
		return answered
	case errors.As(err, &changed):
		return changed
	case errors.As(err, &ue):
		slog.ErrorContext(ctx, "control: pseudo write failed and left behind what it wrote before its transaction", "left", ue.Left, "err", ue)
		return refusal(huma.Error500InternalServerError(fmt.Sprintf("nothing was written, but removing what was written before the transaction failed: %s is left behind", strings.Join(ue.Left, ", "))), "")
	case errors.As(err, &oe):
		return organizationRefusal(oe)
	case errors.As(err, &be):
		return refusal(huma.Error400BadRequest(be.Message), be.Pointer)
	case errors.As(err, &ie):
		r := refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codePlanIssue, Message: fmt.Sprintf("the rows pass one by one but together would raise errors in the plans (%d, listed in issues); nothing was written", len(ie.Issues))},
			HTTPStatus: http.StatusUnprocessableEntity,
		}, "")
		r.Issues = d.pseudoIssues(ctx, ie.Rows, ie.Issues)
		return r
	case errors.Is(err, plan.ErrCheckTimeout):
		return refusal(huma.Error503ServiceUnavailable("checking the rows against the plans took too long; nothing was written, retry the request"), "")
	case errors.Is(err, pseudo.ErrNoPlans):
		return refusal(huma.Error503ServiceUnavailable("the plans are not loaded yet; nothing was written, retry the request"), "")
	}
	pointer := ""
	if errors.As(err, &re) {
		pointer, err = re.Row.Pointer, re.Err
	}
	if storage.IsConflict(err) || errors.Is(err, meta.ErrStaleResourceVersion) {
		return refusal(conflictError("another change touched the same rows at the same time; nothing was written, retry the request"), pointer)
	}
	if storage.IsDuplicate(err) || errors.Is(err, directory.ErrConflict) {
		return refusal(conflictError("another row holds a name or id this row takes: the given metadata.name, or one another change took at the same time; nothing was written"), pointer)
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeClientCanceled, Message: "the request ended before the write did; nothing was written"},
			HTTPStatus: statusClientClosed,
		}, pointer)
	}
	var se huma.StatusError
	if !errors.As(err, &se) && re != nil {
		err = mapWriteErr(re.Row.Kind, re.Row.Meta.ID, err)
	}
	if errors.As(err, &se) && se.GetStatus() < http.StatusInternalServerError {
		return refusal(err, pointer)
	}
	slog.ErrorContext(ctx, "control: pseudo write failed; nothing was written", "pointer", pointer, "err", err)
	return refusal(huma.Error500InternalServerError("the write failed on the server; nothing was written"), pointer)
}

// A request whose client went away before the write ended: no one reads the answer, but the audit row and the access log do.
const (
	codeClientCanceled = "client_canceled"
	statusClientClosed = 499
)

// refusal puts err in the pseudo refusal envelope with pointer.
func refusal(err error, pointer string) *pseudoRefusal {
	var ae *httpapi.APIError
	if inUse, ok := errors.AsType[*resourceInUseError](err); ok {
		return &pseudoRefusal{APIError: inUse.APIError, Pointer: pointer, Blockers: inUse.Blockers}
	}
	if !errors.As(err, &ae) {
		var se huma.StatusError
		status := http.StatusInternalServerError
		if errors.As(err, &se) {
			status = se.GetStatus()
		}
		ae = &httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Message: err.Error()}, HTTPStatus: status}
	}
	return &pseudoRefusal{APIError: *ae, Pointer: pointer}
}

// pseudoIssues projects the plan check's issues on rows for the caller: an issue on one of the new rows (or a plan object of the same id) carries the row's pointer; one on a row the caller may not see keeps its code and kind only.
func (d Deps) pseudoIssues(ctx context.Context, rows []pseudo.Row, issues []engine.Issue) []PseudoIssue {
	ours := make(map[string]pseudo.Row, len(rows))
	for _, r := range rows {
		ours[r.Meta.ID] = r
	}
	var live *engine.View
	if d.PlanView != nil {
		live = d.PlanView()
	}
	out := make([]PseudoIssue, 0, len(issues))
	for _, i := range issues {
		pi := PseudoIssue{Code: i.Code, Kind: string(i.Source.Kind)}
		if r, ok := ours[i.Source.ID]; ok {
			pi.ID, pi.Name, pi.Field, pi.Message, pi.Pointer = r.Meta.ID, r.Meta.Name, i.Field, i.Message, r.Pointer
		} else if live != nil && issueVisible(ctx, d.Authz, plan.KindSingular(i.Source.Kind), i.Source.ID, plan.OwnerOf(live, i.Source)) {
			pi.ID, pi.Name, pi.Field, pi.Message = i.Source.ID, plan.NameOf(live, i.Source), i.Field, i.Message
		} else {
			pi.Message = "a row you may not see would break"
		}
		out = append(out, pi)
	}
	return out
}
