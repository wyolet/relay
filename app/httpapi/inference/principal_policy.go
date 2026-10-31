package inference

import (
	"context"
	"net/http"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policy"
)

// admitCaller answers a principal its caller ladder fails closed: a disabled policy or subject, or an ambiguous binding, on any caller rung. Reports false after writing the response. A disabled policy is an answer, never a miss: nothing falls through to a broader grant.
func admitCaller(w http.ResponseWriter, cp *plan.CallerPlan) bool {
	r := cp.Refused
	if r == nil {
		return true
	}
	switch {
	case r.Code == plan.CodeSubjectDisabled && r.Rung == limits.RungPrincipal:
		// The principal itself is switched off: its credential no longer authenticates.
		writeAuthErr(w, "api key disabled")
	default:
		writeForbidden(w, r.Code, r.Error())
	}
	return false
}

// GoverningPolicy returns the innermost caller policy of the request's authenticated principal, or nil when none is bound. It rejects nothing and writes no response, for endpoints behind AuthenticateMiddleware that accept any valid credential and still honour a policy that has one. A disabled policy is returned: it still governs.
func GoverningPolicy(ctx context.Context) *policy.Policy {
	return PrincipalFrom(ctx).Policy()
}

// WritePolicyDisabled answers a caller whose governing policy is switched off: the 403 inference gives, for endpoints that find the policy with GoverningPolicy.
func WritePolicyDisabled(w http.ResponseWriter) {
	writeForbidden(w, plan.CodePolicyDisabled, "policy is disabled")
}
