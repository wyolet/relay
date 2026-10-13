// POST /access-review answers "may this subject perform this action on this resource, and why?" without performing it. The answer is the authorization decision alone: a request it allows can still fail validation, governance or a reference check.

package control

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/auth/rbac"
)

// accessReviewAction names the audit row a review of someone else's access writes.
const accessReviewAction = "access-review.create"

type reviewSubjectInput struct {
	Kind string `json:"kind" enum:"user,serviceaccount" doc:"What the subject is."`
	Name string `json:"name,omitempty" doc:"Username, or service account slug."`
	ID   string `json:"id,omitempty" doc:"Account id; used instead of name when both are set."`
}

type reviewOwnerInput struct {
	Kind string `json:"kind" enum:"system,user,team,project,host,provider" doc:"Owner kind."`
	Name string `json:"name,omitempty" doc:"Owner slug or username. Omit for system."`
	ID   string `json:"id,omitempty" doc:"Owner id; used instead of name when both are set."`
}

type reviewResourceInput struct {
	Kind  string            `json:"kind" minLength:"1" doc:"API plural (keys, projects, usage, ...) or the singular a handler records."`
	Name  string            `json:"name,omitempty" doc:"Row slug. Omit, with id, to ask about the collection (list, create)."`
	ID    string            `json:"id,omitempty" doc:"Row id; used instead of name when both are set."`
	Owner *reviewOwnerInput `json:"owner,omitempty" doc:"Owner of the row a create would make, or of the rows a list is limited to. A create without one takes the default POST stamps."`
}

type accessReviewInput struct {
	Body struct {
		Subject  *reviewSubjectInput `json:"subject,omitempty" doc:"Who to check. Omit to check yourself; anyone else takes an admin."`
		Action   string              `json:"action" minLength:"1" doc:"The action a handler authorizes, e.g. keys.get. A bare verb takes the resource kind's plural."`
		Resource reviewResourceInput `json:"resource"`
	}
}

type namedRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type reviewedSubject struct {
	Kind     string   `json:"kind" doc:"user, serviceaccount or admin-token."`
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	Admin    bool     `json:"admin" doc:"Holds relay's admin identity, which passes every rule and binding."`
	Disabled bool     `json:"disabled,omitempty" doc:"The account is disabled and cannot sign in; the decision is what it would hold if enabled."`
	Subjects []string `json:"subjects,omitempty" doc:"Binding subjects the decision was evaluated under."`
}

type reviewedResource struct {
	Kind  string    `json:"kind"`
	ID    string    `json:"id,omitempty"`
	Name  string    `json:"name,omitempty"`
	Owner *namedRef `json:"owner,omitempty"`
}

type reviewedBinding struct {
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Subject namedRef `json:"subject" doc:"The binding subject the caller matched."`
	Role    namedRef `json:"role"`
	Scope   namedRef `json:"scope"`
}

type accessReview struct {
	Allowed          bool             `json:"allowed"`
	Reason           string           `json:"reason" enum:"admin,rule,binding,credential,none" doc:"What decided. Only the first match is reported."`
	Rule             string           `json:"rule,omitempty" doc:"The relay rule that decided: personal-row, catalog-read, shared-policy-read, system-role-read, scoped-list or parent-team-read."`
	Binding          *reviewedBinding `json:"binding,omitempty" doc:"The role binding that granted the action."`
	CredentialDenied bool             `json:"credentialDenied,omitempty" doc:"The subject holds the permission but the credential's limits refuse it."`
	Chain            []namedRef       `json:"chain,omitempty" doc:"The resource's scope chain, most specific first, when the decision read it."`
	Subject          reviewedSubject  `json:"subject"`
	Action           string           `json:"action"`
	Resource         reviewedResource `json:"resource"`
}

type accessReviewOutput struct {
	Body accessReview
}

// accessReviewer resolves a review's subject and resource the way authentication and the resource's own routes do, then asks the authorizer to explain.
type accessReviewer struct {
	authz   authz.Authorizer
	catalog *appcatalog.Catalog
	users   userFinder
	rows    kindIndex
}

func registerAccessReview(api huma.API, d Deps, protect huma.Middlewares, rows kindIndex) {
	r := accessReviewer{authz: d.Authz, catalog: d.Catalog, rows: rows}
	if d.Users != nil {
		r.users = d.Users
		rows.add(userRows(d.Users))
	}
	r.register(api, protect)
}

func (r accessReviewer) register(api huma.API, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_access_review",
		Method:      http.MethodPost,
		Path:        "/access-review",
		Summary:     "Check whether a subject may perform an action, and why",
		Description: "Explains the authorization decision the real request would get, without performing it. Checking yourself is open to any signed-in caller; checking another user or a service account takes an admin and is audited.",
		Tags:        []string{"auth"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500, 501},
	}, r.review)
}

func (r accessReviewer) review(ctx context.Context, in *accessReviewInput) (*accessReviewOutput, error) {
	caller := actor.From(ctx)
	req := in.Body
	self := req.Subject == nil || namesCaller(caller, *req.Subject)
	if self {
		// What you may do is what your own requests already show you.
		audit.Discard(ctx)
	} else {
		named := audit.Resource{Kind: req.Subject.Kind, ID: req.Subject.ID, Name: req.Subject.Name}
		if err := refcheck.RequireAdmin(ctx, r.authz, "checking another subject's access"); err != nil {
			audit.Record(ctx, accessReviewAction, named, audit.StatusDenied)
			return nil, huma.Error403Forbidden(err.Error())
		}
		audit.Record(ctx, accessReviewAction, named, audit.StatusAllowed)
	}

	subject, shown := caller, r.callerShown(ctx, caller)
	if !self {
		var err error
		if subject, shown, err = r.subject(ctx, *req.Subject); err != nil {
			return nil, err
		}
		audit.Record(ctx, accessReviewAction, audit.Resource{Kind: shown.Kind, ID: shown.ID, Name: shown.Name}, audit.StatusAllowed)
	}

	action := r.action(req.Action, req.Resource.Kind)
	_, verb := rbac.SplitAction(action)
	res, shownRes, err := r.resource(ctx, subject, verb, req.Resource)
	if err != nil {
		return nil, err
	}

	explainer, ok := r.authz.(authz.Explainer)
	if !ok {
		return nil, errNoAccessReview
	}
	x, err := explainer.Explain(ctx, subject, action, res)
	switch {
	case errors.Is(err, authz.ErrNoExplanation):
		return nil, errNoAccessReview
	case err != nil:
		return nil, huma.Error500InternalServerError("authz: " + err.Error())
	}
	return &accessReviewOutput{Body: r.render(ctx, x, shown, action, shownRes)}, nil
}

var errNoAccessReview = huma.NewError(http.StatusNotImplemented,
	"access review needs RELAY_AUTHZ=rbac; under single every signed-in caller may perform every action")

// action is the action as handlers name it: a bare verb takes the resource kind's API plural.
func (r accessReviewer) action(action, kind string) string {
	if strings.Contains(action, ".") {
		return action
	}
	if kr, ok := r.rows[kind]; ok {
		kind = kr.plural
	}
	return kind + "." + action
}

func (r accessReviewer) snapshot() *appcatalog.Snapshot {
	if r.catalog == nil {
		return nil
	}
	return r.catalog.Current()
}
