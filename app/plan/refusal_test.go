package plan_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/usage"
)

// Every code relay refuses with has exactly one class; codes that are not relay refusals have none.
func TestKindOf_EveryCode(t *testing.T) {
	for code, want := range map[string]plan.RefusalKind{
		plan.CodePolicyDisabled:      plan.RefusalModelNotAllowed,
		plan.CodeModelNotAllowed:     plan.RefusalModelNotAllowed,
		plan.CodeUnpricedUnderBudget: plan.RefusalModelNotAllowed,
		plan.CodeSubjectDisabled:     plan.RefusalModelNotAllowed,
		plan.CodeAccountClosed:       plan.RefusalModelNotAllowed,
		plan.CodeAccountNotHeld:      plan.RefusalModelNotAllowed,
		plan.CodeDuplicateBinding:    plan.RefusalModelNotAllowed,
		plan.CodeModelNotFound:       plan.RefusalHostNotReachable,
		plan.CodeModelDisabled:       plan.RefusalHostNotReachable,
		plan.CodeNoBinding:           plan.RefusalHostNotReachable,
		plan.CodeNoCredentials:       plan.RefusalHostNotReachable,
		plan.CodeRateLimitExceeded:   plan.RefusalNoCapacity,
		plan.CodeBudgetExceeded:      plan.RefusalNoCapacity,
		plan.CodeUpstreamLimited:     plan.RefusalNoCapacity,
		plan.CodeNoHealthyKeys:       plan.RefusalNoCapacity,

		"upstream_error":        "",
		"upstream_unreachable":  "",
		"upstream_unavailable":  "",
		"passthrough_forbidden": "",
		"overloaded":            "",
		"":                      "",
	} {
		if got := plan.KindOf(code); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", code, got, want)
		}
	}
	if got := (&plan.Refusal{Code: plan.CodeNoBinding}).Kind(); got != plan.RefusalHostNotReachable {
		t.Errorf("Refusal.Kind = %q", got)
	}
}

func TestLimitCode(t *testing.T) {
	budget := &limits.Refused{Counter: &limits.Counter{Kind: limits.KindBudget}}
	rule := &limits.Refused{Counter: &limits.Counter{}}
	if plan.LimitCode(budget) != plan.CodeBudgetExceeded || plan.LimitCode(rule) != plan.CodeRateLimitExceeded {
		t.Fatalf("LimitCode = %q, %q", plan.LimitCode(budget), plan.LimitCode(rule))
	}
}

// The error envelope and the usage event document the refusal as an enum of exactly these classes.
func TestRefusalEnumsMatchTheClasses(t *testing.T) {
	want := []string{string(plan.RefusalModelNotAllowed), string(plan.RefusalHostNotReachable), string(plan.RefusalNoCapacity)}
	for _, typ := range []reflect.Type{reflect.TypeFor[httpapi.APIErrorBody](), reflect.TypeFor[usage.Event]()} {
		f, ok := typ.FieldByName("Refusal")
		if !ok {
			t.Fatalf("%s has no Refusal field", typ)
		}
		if got := strings.Split(f.Tag.Get("enum"), ","); !slices.Equal(got, want) {
			t.Errorf("%s.Refusal enum = %v, want %v", typ, got, want)
		}
	}
}
