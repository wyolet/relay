// The HTTP side of the pseudo kinds (app/pseudo): which kinds RELAY_PSEUDO_KINDS turns on, the create each real row kind goes through, and how a refused pseudo object is answered. Each kind's route lives in its own pseudo_<kind>.go.

package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// pseudoKinds registers each pseudo kind's route, by the name RELAY_PSEUDO_KINDS turns it on with.
var pseudoKinds = map[string]func(huma.API, Deps, huma.Middlewares){
	"agents": registerPseudoAgent,
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

func createdRows(rows []pseudo.Row) []CreatedRow {
	out := make([]CreatedRow, len(rows))
	for i, r := range rows {
		out[i] = CreatedRow{Kind: r.Kind, ID: r.Meta.ID, Name: r.Meta.Name}
	}
	return out
}

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
	Pointer string        `json:"pointer,omitempty" doc:"JSON pointer of the part of the body the refused row came from, such as /spec/access/accountIds/1."`
	Issues  []PseudoIssue `json:"issues,omitempty"  doc:"With plan_issue: each error the rows would raise in the plans that they do not have now."`
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

// documentPlanIssue documents the 422 of the pseudo route at path once it is registered: huma writes its own 422 for a body over the operation's responses.
func documentPlanIssue(api huma.API, path string) {
	if item := api.OpenAPI().Paths[path]; item != nil && item.Post != nil {
		item.Post.Responses["422"] = jsonResponse(api, "plan_issue: every row passes its own checks, but together they would raise errors in the plans that are not there now; issues lists them, each with the pointer of the part of the body it came from. Nothing was written. Also a body that does not match the schema, with error.details.", pseudoRefusal{})
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
	}
	return func(string) bool { return false }
}

// pseudoWrite is one pseudo object's write: it creates each row as the row's own create route would, and keeps the audit rows and key plaintexts of what it made.
type pseudoWrite struct {
	d          Deps
	audit      []audit.Row
	plaintexts map[string]string
}

// runPseudo writes the rows expand returns in one transaction (pseudo.Writer); a dry run writes nothing and records no audit row.
func (d Deps) runPseudo(ctx context.Context, expand pseudo.ExpandFunc, dryRun bool) (*pseudoWrite, []pseudo.Row, error) {
	w := &pseudoWrite{d: d, plaintexts: map[string]string{}}
	writer := pseudo.Writer{Stores: d.Stores, Check: d.PlanCheck, View: d.PlanView}
	rows, err := writer.Run(ctx, expand, w.create, dryRun)
	if err != nil {
		return nil, nil, d.pseudoError(ctx, err)
	}
	if dryRun {
		audit.Discard(ctx)
	} else {
		audit.RecordEach(ctx, w.audit)
	}
	return w, rows, nil
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
	case *policybinding.PolicyBinding:
		_, _, err = d.createBinding(ctx, v)
	case *key.Key:
		var plaintext string
		plaintext, err = d.createKey(ctx, v, createKeySpec{AccountID: v.Spec.AccountID, ExpiresAt: v.Spec.ExpiresAt, Enabled: v.Spec.Enabled, PassthroughAllowed: v.Spec.PassthroughAllowed, PayloadLoggingEnabled: v.Spec.PayloadLoggingEnabled})
		w.plaintexts[v.Meta.ID] = plaintext
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
	}
	return fmt.Errorf("account %q: only a share or a caller account is made here", a.Meta.Name)
}

func (d Deps) createPseudoShare(ctx context.Context, share *account.Account) error {
	src, err := d.Stores.Account.Get(ctx, share.Spec.SourceAccountID)
	if err != nil {
		return err
	}
	if src == nil || !visibleTo(ctx, d.Authz, "account", src.Meta.ID, src.Meta.Owner) {
		return huma.Error404NotFound(fmt.Sprintf("account %q not found", share.Spec.SourceAccountID))
	}
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
	if err := d.Stores.Account.Upsert(ctx, share); err != nil {
		if errors.Is(err, account.ErrShareTaken) {
			return refErr(c.ShareExists(ctx, src, share.Receiver()))
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
	switch {
	case errors.As(err, &answered):
		return answered
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
	if re != nil {
		var se huma.StatusError
		if !errors.As(err, &se) {
			err = mapWriteErr(re.Row.Kind, re.Row.Meta.ID, err)
		}
	}
	return refusal(err, pointer)
}

// refusal puts err in the pseudo refusal envelope with pointer.
func refusal(err error, pointer string) *pseudoRefusal {
	var ae *httpapi.APIError
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
