package inference

import (
	"context"
	"net/http"

	"github.com/wyolet/relay/app/account"
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
	if a := edgeAnswer(r); a.Status == http.StatusUnauthorized {
		writeAuthErr(w, a.Message)
	} else {
		writeForbidden(w, a.Code, a.Message)
	}
	return false
}

// edgeAnswer is relay's answer to a credential its lane refuses, which its usage row records too: a 403 naming the refusal, or a 401 when the principal itself is switched off, since its credential no longer authenticates.
func edgeAnswer(r *plan.Refusal) plan.Answer {
	if r.Code == plan.CodeSubjectDisabled && r.Rung == limits.RungLane {
		return plan.Answer{Status: http.StatusUnauthorized, Type: "invalid_request_error", Code: "unauthenticated", Kind: r.Kind(), Message: "api key disabled"}
	}
	return plan.Answer{Status: http.StatusForbidden, Type: "invalid_request_error", Code: r.Code, Kind: r.Kind(), Message: r.Error()}
}

// GoverningPolicy returns the innermost caller policy of the request's authenticated principal, or nil when none is bound. It rejects nothing and writes no response, for endpoints behind AuthenticateMiddleware that accept any valid credential and still honour a policy that has one. A disabled policy is returned: it still governs.
func GoverningPolicy(ctx context.Context) *policy.Policy {
	return PrincipalFrom(ctx).Policy()
}

// PayloadLoggingOf returns the payload-logging say of the request's lane: the nearest account on its ladder that sets one, "" when none does or no credential was presented.
func PayloadLoggingOf(ctx context.Context) account.PayloadLogging {
	if p := PrincipalFrom(ctx); p != nil && p.Caller != nil {
		return p.Caller.PayloadLogging
	}
	return ""
}

// WritePolicyDisabled answers a caller whose governing policy is switched off: the 403 inference gives, for endpoints that find the policy with GoverningPolicy.
func WritePolicyDisabled(w http.ResponseWriter) {
	writeForbidden(w, plan.CodePolicyDisabled, "policy is disabled")
}
