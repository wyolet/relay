package control

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
)

// planIssueAuthorize is intentionally absent: "plan-issues" is not in the role
// vocabulary (app/role/role.go), so a per-kind RBAC gate would block every
// non-wildcard principal. Authentication is enforced by the protect middleware;
// visibility is enforced row-by-row inside planIssues.

// PlanIssueRef is a related object implicated in one plan issue.
type PlanIssueRef struct {
	Kind string `json:"kind" doc:"Kind of the referenced object (e.g. policy, host)."`
	ID   string `json:"id"   doc:"Id (UUIDv7) of the referenced object; empty when the caller may not see the object."`
	Name string `json:"name" doc:"Slug of the referenced object (a user's username); empty when the caller may not see it or it no longer exists."`
}

// PlanIssue is one diagnostic produced by the plan engine for a relay object.
type PlanIssue struct {
	Code     string         `json:"code"     doc:"Stable snake_case diagnostic identifier, such as ref_missing or host_mismatch."`
	Message  string         `json:"message"  doc:"Human-readable explanation of what is wrong."`
	Kind     string         `json:"kind"     doc:"Kind of the object carrying the issue (e.g. key, policybinding)."`
	ID       string         `json:"id"       doc:"Id (UUIDv7) of the object carrying the issue."`
	Name     string         `json:"name"     doc:"Slug of the object; empty for derived objects that have no slug."`
	Field    string         `json:"field"    doc:"Spec path of the object the issue is about, such as spec.models[2] or spec.poolIds[0]; empty when it is about the object as a whole."`
	Severity string         `json:"severity" enum:"error,warning" doc:"Severity: error means the object cannot serve; warning means it serves but something is likely wrong."`
	Refs     []PlanIssueRef `json:"refs"     doc:"Objects the issue implicates, such as a missing target."`
}

type planIssuesInput struct {
	Kind   string   `query:"kind"   optional:"true" doc:"Filter by object kind, such as key or policybinding. Omit for all kinds."`
	ID     string   `query:"id"     optional:"true" doc:"Filter by object id (UUIDv7). Takes effect only together with kind."`
	Target string   `query:"target" optional:"true" pattern:"^[a-z]+:[^:]+$" doc:"Only issues whose refs name this object, as kind:id (e.g. ratelimit:<id>): the issues other objects have because of it."`
	About  []string `query:"about,explode" doc:"Only issues on one of these objects or naming one in their refs, each as kind:id (repeatable, any may match): a policy page passes the policy and its pools to get every issue it shows in one call."`
}

// issueFilter is the query's filters; an issue passes when it passes every one set.
type issueFilter struct {
	kind, id string
	target   engine.Ref
	about    []engine.Ref
}

func (f issueFilter) keeps(iss engine.Issue) bool {
	switch {
	case f.kind != "" && string(iss.Source.Kind) != f.kind,
		f.kind != "" && f.id != "" && iss.Source.ID != f.id,
		f.target != (engine.Ref{}) && iss.Target != f.target,
		len(f.about) > 0 && !slices.ContainsFunc(f.about, func(r engine.Ref) bool { return iss.Source == r || iss.Target == r }):
		return false
	}
	return true
}

// parseAbout reads each about value as kind:id; an error names the first malformed one.
func parseAbout(raw []string) ([]engine.Ref, error) {
	out := make([]engine.Ref, 0, len(raw))
	for _, s := range raw {
		kind, id, ok := strings.Cut(s, ":")
		if !ok || kind == "" || id == "" || strings.Contains(id, ":") {
			return nil, huma.Error422UnprocessableEntity("about " + s + " is not kind:id")
		}
		out = append(out, engine.Ref{Kind: engine.Kind(kind), ID: id})
	}
	return out, nil
}

// parseObjectRef reads "kind:id"; the zero Ref for "".
func parseObjectRef(s string) engine.Ref {
	kind, id, _ := strings.Cut(s, ":")
	return engine.Ref{Kind: engine.Kind(kind), ID: id}
}

type planIssuesOutput struct {
	Body []PlanIssue
}

func registerPlanIssues(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "plan_issues_list",
		Method:      "GET",
		Path:        "/plan-issues",
		Summary:     "List plan-engine diagnostics",
		Description: "Returns every issue the plan engine has recorded for relay objects (keys, policies, bindings, …) that the caller may see, sorted by kind, name and code. Errors mean the object cannot serve; warnings mean it serves but something is likely wrong. Filter by kind, or by kind and id to narrow to one object; target keeps the issues naming an object in their refs, and about (repeatable) the issues on any of the given objects or naming one. Filters combine.",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{401, 422, 500},
	}, func(ctx context.Context, in *planIssuesInput) (*planIssuesOutput, error) {
		about, err := parseAbout(in.About)
		if err != nil {
			return nil, err
		}
		if d.PlanView == nil {
			return &planIssuesOutput{Body: []PlanIssue{}}, nil
		}
		v := d.PlanView()
		if v == nil {
			return &planIssuesOutput{Body: []PlanIssue{}}, nil
		}
		f := issueFilter{kind: in.Kind, id: in.ID, target: parseObjectRef(in.Target), about: about}
		return &planIssuesOutput{Body: planIssues(ctx, d.Authz, v, f)}, nil
	})
}

// planIssues projects v's issues into the API shape, filtering to rows the
// caller may read. An id without a kind is ambiguous across kinds and is
// ignored. A ref the caller may not see keeps its kind but has its id and
// name cleared.
func planIssues(ctx context.Context, az authz.Authorizer, v *engine.View, f issueFilter) []PlanIssue {
	var raw []engine.Issue
	if f.kind != "" && f.id != "" {
		raw = v.Issues(engine.Ref{Kind: engine.Kind(f.kind), ID: f.id})
	} else {
		raw = v.AllIssues()
	}
	raw = slices.DeleteFunc(raw, func(iss engine.Issue) bool { return !f.keeps(iss) })
	out := make([]PlanIssue, 0, len(raw))
	for _, iss := range raw {
		singular := plan.KindSingular(iss.Source.Kind)
		owner := plan.OwnerOf(v, iss.Source)
		if !issueVisible(ctx, az, singular, iss.Source.ID, owner) {
			continue
		}
		refs := []PlanIssueRef{}
		if iss.Target.Kind != "" {
			ref := PlanIssueRef{Kind: string(iss.Target.Kind)}
			if issueVisible(ctx, az, plan.KindSingular(iss.Target.Kind), iss.Target.ID, plan.OwnerOf(v, iss.Target)) {
				ref.ID, ref.Name = iss.Target.ID, plan.NameOf(v, iss.Target)
			}
			refs = append(refs, ref)
		}
		out = append(out, PlanIssue{
			Code:     iss.Code,
			Message:  iss.Message,
			Kind:     string(iss.Source.Kind),
			ID:       iss.Source.ID,
			Name:     plan.NameOf(v, iss.Source),
			Field:    iss.Field,
			Severity: severityString(iss.Severity),
			Refs:     refs,
		})
	}
	slices.SortStableFunc(out, func(a, b PlanIssue) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Code, b.Code))
	})
	return out
}

// issueVisible reports whether the caller may see a row of singular kind with
// the given id and owner. When singular is "" (a derived kind with no control
// API representation), a Scoper restricts visibility to admins only; without a
// Scoper the row is shown as-is.
func issueVisible(ctx context.Context, az authz.Authorizer, singular, id string, owner meta.Owner) bool {
	if singular == "" {
		_, scoped := az.(authz.Scoper)
		return !scoped || authz.IsAdmin(ctx)
	}
	return visibleTo(ctx, az, singular, id, owner)
}

func severityString(s engine.Severity) string {
	if s == engine.SeverityWarning {
		return "warning"
	}
	return "error"
}
