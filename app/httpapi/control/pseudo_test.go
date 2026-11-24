package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/httpapi/openapitest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// hideKind is an authorizer that allows everything and hides the rows of one kind.
type hideKind struct{ kind string }

func (hideKind) Authorize(context.Context, string, authz.Resource) error { return nil }
func (h hideKind) Visible(_ context.Context, kind, _ string, _ meta.Owner) bool {
	return kind != h.kind
}

// A refused pseudo object answers in the error envelope with the pointer of the row at fault; a write conflict answers 409 conflict; the plan check's refusals answer 422 with the issues, or 503.
func TestPseudoError(t *testing.T) {
	httpapi.Install()
	w := &plantest.World{}
	team := w.Team()
	proj := w.Project(team)
	sa := w.ServiceAccount(proj)
	plans := w.Load(t)
	lane := w.ServiceAccountAccount(sa)
	teamAcct := w.TeamAccount(team)
	ours := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "ci-key"}}
	rows := []pseudo.Row{{Kind: "key", Pointer: "/spec/key", Meta: &ours.Meta, Value: ours}}
	d := Deps{Authz: hideKind{kind: "account"}, PlanView: plans.Engine.View}

	keyRow := rows[0]
	apiErr := func(status int, code, msg string) httpapi.APIError {
		typ := "invalid_request_error"
		if status == http.StatusInternalServerError {
			typ, code = "server_error", "internal_error"
		}
		return httpapi.APIError{Err: httpapi.APIErrorBody{Type: typ, Code: code, Message: msg}, HTTPStatus: status}
	}
	const conflictMsg = "another change touched the same rows at the same time; nothing was written, retry the request"
	const dupMsg = "another row holds a name or id this row takes: the given metadata.name, or one another change took at the same time; nothing was written"
	const serverFailed = "the write failed on the server; nothing was written"
	for _, tc := range []struct {
		name string
		err  error
		want *pseudoRefusal
	}{
		{"a body that expands to nothing", &pseudo.BodyError{Pointer: "/spec", Message: "exactly one"},
			&pseudoRefusal{APIError: apiErr(400, "", "exactly one"), Pointer: "/spec"}},
		{"an account refused while reading", refusal(huma.Error404NotFound(`account "x" not found`), "/spec/access/accountIds/2"),
			&pseudoRefusal{APIError: apiErr(404, "", `account "x" not found`), Pointer: "/spec/access/accountIds/2"}},
		{"a row its checks refuse", &pseudo.RowError{Row: keyRow, Err: huma.Error403Forbidden("forbidden")},
			&pseudoRefusal{APIError: apiErr(403, "", "forbidden"), Pointer: "/spec/key"}},
		{"a row a refcheck code refuses", &pseudo.RowError{Row: keyRow, Err: refErr(&refcheck.Error{Status: 409, Code: refcheck.CodeShareExists, Msg: "shared already"})},
			&pseudoRefusal{APIError: apiErr(409, refcheck.CodeShareExists, "shared already"), Pointer: "/spec/key"}},
		{"a concurrent transaction", &pseudo.RowError{Row: keyRow, Err: &pgconn.PgError{Code: "40001"}},
			&pseudoRefusal{APIError: apiErr(409, "conflict", conflictMsg), Pointer: "/spec/key"}},
		{"a commit that lost", fmt.Errorf("storage: commit: %w", &pgconn.PgError{Code: "40P01"}),
			&pseudoRefusal{APIError: apiErr(409, "conflict", conflictMsg)}},
		{"a name taken", &pseudo.RowError{Row: keyRow, Err: fmt.Errorf("upsert: %w: service_accounts_name_key", directory.ErrConflict)},
			&pseudoRefusal{APIError: apiErr(409, "conflict", dupMsg), Pointer: "/spec/key"}},
		{"a store failing under a row", &pseudo.RowError{Row: keyRow, Err: errors.New("connection reset")},
			&pseudoRefusal{APIError: apiErr(500, "", serverFailed), Pointer: "/spec/key"}},
		{"a row check failing on the server", &pseudo.RowError{Row: keyRow, Err: huma.Error500InternalServerError("hostkey.Get: dial tcp 10.0.0.5:5432")},
			&pseudoRefusal{APIError: apiErr(500, "", serverFailed), Pointer: "/spec/key"}},
		{"a store failing while reading", fmt.Errorf("account.List: %w", errors.New("conn busy")),
			&pseudoRefusal{APIError: apiErr(500, "", serverFailed)}},
		{"a transaction that did not begin", errors.New("storage: begin: dial tcp: i/o timeout"),
			&pseudoRefusal{APIError: apiErr(500, "", serverFailed)}},
		{"a scratch build failing", errors.New("plan: check: engine: list account: boom"),
			&pseudoRefusal{APIError: apiErr(500, "", serverFailed)}},
		{"the client went away", fmt.Errorf("plan check: %w", context.Canceled),
			&pseudoRefusal{APIError: httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeClientCanceled, Message: "the request ended before the write did; nothing was written"}, HTTPStatus: statusClientClosed}}},
		{"a slow check", plan.ErrCheckTimeout,
			&pseudoRefusal{APIError: apiErr(503, "", "checking the rows against the plans took too long; nothing was written, retry the request")}},
		{"no plans", pseudo.ErrNoPlans,
			&pseudoRefusal{APIError: apiErr(503, "", "the plans are not loaded yet; nothing was written, retry the request")}},
		{"plan issues", &pseudo.IssuesError{Rows: rows, Issues: []engine.Issue{
			{Severity: engine.SeverityError, Code: plan.IssueRefMissing, Source: engine.Ref{Kind: plan.KindKey, ID: ours.Meta.ID}, Field: "spec.accountId", Message: "names an account that was deleted"},
			{Severity: engine.SeverityError, Code: "duplicate_name", Source: engine.Ref{Kind: plan.KindServiceAccount, ID: sa.Meta.ID}, Field: "metadata.name", Message: "shares its name"},
			{Severity: engine.SeverityError, Code: "duplicate_name", Source: engine.Ref{Kind: plan.KindAccount, ID: lane.Meta.ID}, Field: "metadata.name", Message: "shares its name with " + teamAcct.Meta.Name},
		}}, &pseudoRefusal{
			APIError: apiErr(422, codePlanIssue, "the rows pass one by one but together would raise errors in the plans (3, listed in issues); nothing was written"),
			Issues: []PseudoIssue{
				{Code: plan.IssueRefMissing, Message: "names an account that was deleted", Kind: "key", ID: ours.Meta.ID, Name: "ci-key", Field: "spec.accountId", Pointer: "/spec/key"},
				{Code: "duplicate_name", Message: "shares its name", Kind: "serviceaccount", ID: sa.Meta.ID, Name: sa.Meta.Name, Field: "metadata.name"},
				{Code: "duplicate_name", Message: "a row you may not see would break", Kind: "account"},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := d.pseudoError(t.Context(), tc.err)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("answer (-want +got):\n%s", diff)
			}
		})
	}
}

// A pseudo kind RELAY_PSEUDO_KINDS does not name has no route: absent from the document and 404 on its path. Named, it is documented in full.
func TestPseudoKinds_FlagGatesTheRoutes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kinds  []string
		served bool
		status int
	}{
		{"off", nil, false, http.StatusNotFound},
		{"another kind's name", []string{"gadgets"}, false, http.StatusNotFound},
		{"by name", []string{"agents"}, true, http.StatusUnauthorized},
		{"all", []string{"all"}, true, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := mountDeps(t)
			d.PseudoKinds = tc.kinds
			r := chi.NewRouter()
			api := Mount(r, d)
			doc, err := openapitest.Decode(api.OpenAPI())
			if err != nil {
				t.Fatal(err)
			}
			paths, _ := doc["paths"].(map[string]any)
			if _, ok := paths["/pseudo-agents"]; ok != tc.served {
				t.Errorf("/pseudo-agents in the document: %v, want %v", ok, tc.served)
			}
			if tc.served {
				for _, p := range openapitest.Problems(doc) {
					t.Error(p)
				}
				if ref := responseRef(operation(t, doc, "/pseudo-agents", "post"), "422"); ref != "#/components/schemas/pseudoRefusal" {
					t.Errorf("422 answers %q, want pseudoRefusal", ref)
				}
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pseudo-agents", strings.NewReader(`{}`)))
			if rec.Code != tc.status {
				t.Errorf("POST /pseudo-agents: %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
		})
	}
}
