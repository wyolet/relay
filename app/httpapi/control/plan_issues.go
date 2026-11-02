package control

import (
	"cmp"
	"context"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
)

// PlanIssueRef is a related object implicated in one plan issue.
type PlanIssueRef struct {
	Kind string `json:"kind" doc:"Kind of the referenced object (e.g. policy, host)."`
	ID   string `json:"id"   doc:"Id (UUIDv7) of the referenced object."`
}

// PlanIssue is one diagnostic produced by the plan engine for a catalog object.
type PlanIssue struct {
	Code     string         `json:"code"     doc:"Stable snake_case diagnostic identifier, such as ref_missing or host_mismatch."`
	Message  string         `json:"message"  doc:"Human-readable explanation of what is wrong."`
	Kind     string         `json:"kind"     doc:"Kind of the object carrying the issue (e.g. key, policybinding)."`
	ID       string         `json:"id"       doc:"Id (UUIDv7) of the object carrying the issue."`
	Name     string         `json:"name"     doc:"Slug of the object; empty for derived objects that have no slug."`
	Severity string         `json:"severity" enum:"error,warning" doc:"Severity: error means the object cannot serve; warning means it serves but something is likely wrong."`
	Refs     []PlanIssueRef `json:"refs"     doc:"Objects the issue implicates, such as a missing target."`
}

type planIssuesInput struct {
	Kind string `query:"kind" optional:"true" doc:"Filter by object kind, such as key or policybinding. Omit for all kinds."`
	ID   string `query:"id"   optional:"true" doc:"Filter by object id (UUIDv7). Takes effect only together with kind."`
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
		Description: "Returns every issue the plan engine has recorded for catalog objects, sorted by kind, name and code. Errors mean an object cannot serve; warnings mean it serves but something is likely wrong. Filter by kind, or by kind and id to narrow to one object.",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, in *planIssuesInput) (*planIssuesOutput, error) {
		if err := d.Authz.Authorize(ctx, "plan-issues.list", authz.Resource{Kind: "plan-issues"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if d.PlanView == nil {
			return &planIssuesOutput{Body: []PlanIssue{}}, nil
		}
		v := d.PlanView()
		if v == nil {
			return &planIssuesOutput{Body: []PlanIssue{}}, nil
		}
		return &planIssuesOutput{Body: planIssues(v, in.Kind, in.ID)}, nil
	})
}

// planIssues projects v's issues for the API. An id without a kind is ambiguous across kinds, so it filters nothing.
func planIssues(v *engine.View, kind, id string) []PlanIssue {
	var raw []engine.Issue
	if kind != "" && id != "" {
		raw = v.Issues(engine.Ref{Kind: engine.Kind(kind), ID: id})
	} else {
		raw = v.AllIssues()
		if kind != "" {
			raw = slices.DeleteFunc(raw, func(iss engine.Issue) bool { return string(iss.Source.Kind) != kind })
		}
	}
	out := make([]PlanIssue, 0, len(raw))
	for _, iss := range raw {
		refs := []PlanIssueRef{}
		if iss.Target.Kind != "" {
			refs = append(refs, PlanIssueRef{Kind: string(iss.Target.Kind), ID: iss.Target.ID})
		}
		out = append(out, PlanIssue{
			Code:     iss.Code,
			Message:  iss.Message,
			Kind:     string(iss.Source.Kind),
			ID:       iss.Source.ID,
			Name:     plan.NameOf(v, iss.Source),
			Severity: severityString(iss.Severity),
			Refs:     refs,
		})
	}
	slices.SortStableFunc(out, func(a, b PlanIssue) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Code, b.Code))
	})
	return out
}

func severityString(s engine.Severity) string {
	if s == engine.SeverityWarning {
		return "warning"
	}
	return "error"
}
