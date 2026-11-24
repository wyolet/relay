// Running a pseudo request: the names its rows mint, what it records for the plan check, the write in one transaction with its audit rows, and the create each real row kind goes through.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
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
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

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
