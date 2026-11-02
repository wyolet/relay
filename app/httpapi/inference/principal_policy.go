package inference

import (
	"context"
	"net/http"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
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

// AdmitCaller refuses the authenticated caller of ctx exactly when its lane refuses an inference request: a closed account, a lane its credential may not use, a disabled policy, account, team, project or principal on any rung. For endpoints behind AuthenticateMiddleware, which route nothing but must not take a refused caller's report either. Returns the refusal after writing the answer inference gives it; nil for an admitted or anonymous caller.
func AdmitCaller(ctx context.Context, w http.ResponseWriter) *plan.Refusal {
	p := PrincipalFrom(ctx)
	if p == nil || p.Caller == nil || admitCaller(w, p.Caller) {
		return nil
	}
	return p.Caller.Refused
}

// PayloadLoggingOf returns the payload-logging say of the request's lane: the nearest account on its ladder that sets one, "" when none does or no credential was presented.
func PayloadLoggingOf(ctx context.Context) account.PayloadLogging {
	if p := PrincipalFrom(ctx); p != nil && p.Caller != nil {
		return p.Caller.PayloadLogging
	}
	return ""
}
