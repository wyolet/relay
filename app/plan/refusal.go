package plan

import (
	"fmt"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/engine"
)

// Refusal says why a lane, a credential or a route cannot serve: a stable code, and the rung, account and policy that decided it.
type Refusal struct {
	Code      string
	Rung      limits.Rung
	SubjectID string
	PolicyID  string
	Message   string
}

func (r *Refusal) Error() string {
	if r.Rung == "" {
		return r.Code + ": " + r.Message
	}
	return fmt.Sprintf("%s: %s (%s rung %s, policy %s)", r.Code, r.Message, r.Rung, r.SubjectID, r.PolicyID)
}

// Refusal codes. A refusal on a rung names that rung; model and binding refusals name none.
const (
	CodePolicyDisabled      = "policy_disabled"
	CodeDuplicateBinding    = "duplicate_binding"
	CodeSubjectDisabled     = "subject_disabled"
	CodeModelNotFound       = "model_not_found"
	CodeModelDisabled       = "model_disabled"
	CodeNoBinding           = "no_binding"
	CodeModelNotAllowed     = "model_not_allowed"
	CodeNoCredentials       = "no_credentials"
	CodeUnpricedUnderBudget = "unpriced_under_budget"
	// CodeAccountClosed refuses a member account whose membership ended.
	CodeAccountClosed = "account_closed"
	// CodeAccountNotHeld refuses a lane the key's principal does not hold.
	CodeAccountNotHeld = "account_not_held"
)

// Codes relay refuses with once a route is found: a caller limit, or no credential of the route with room.
const (
	CodeRateLimitExceeded = "rate_limit_exceeded"
	CodeBudgetExceeded    = "budget_exceeded"
	// CodeUpstreamLimited is every credential over an upstream limit or in its account's cooldown; answered as rate_limit_exceeded.
	CodeUpstreamLimited = "upstream_limited"
	// CodeNoHealthyKeys is every credential of the route out of rotation, its breaker open.
	CodeNoHealthyKeys = "no_healthy_keys"
	// CodeUpstreamUnreachable is a dial failure; it is a refusal only when no candidate host of the request accepted a connection.
	CodeUpstreamUnreachable = "upstream_unreachable"
)

// RefusalKind is the class of a refusal a client acts on: use another model, reach another host, or wait.
type RefusalKind string

const (
	RefusalModelNotAllowed  RefusalKind = "model_not_allowed"
	RefusalHostNotReachable RefusalKind = "host_not_reachable"
	RefusalNoCapacity       RefusalKind = "no_capacity"
)

// KindOf is the class of a refusal code; "" for a code that is not a relay refusal, such as an upstream's own error.
func KindOf(code string) RefusalKind {
	switch code {
	case CodePolicyDisabled, CodeModelNotAllowed, CodeUnpricedUnderBudget, CodeSubjectDisabled, CodeAccountClosed, CodeAccountNotHeld, CodeDuplicateBinding:
		return RefusalModelNotAllowed
	case CodeModelNotFound, CodeModelDisabled, CodeNoBinding, CodeNoCredentials, CodeUpstreamUnreachable:
		return RefusalHostNotReachable
	case CodeRateLimitExceeded, CodeBudgetExceeded, CodeUpstreamLimited, CodeNoHealthyKeys:
		return RefusalNoCapacity
	}
	return ""
}

// Kind is r's class.
func (r *Refusal) Kind() RefusalKind { return KindOf(r.Code) }

// LimitCode is the code a refused counter answers with: budget_exceeded for a spent budget, rate_limit_exceeded for any other.
func LimitCode(r *limits.Refused) string {
	if r.Counter != nil && r.Counter.Kind == limits.KindBudget {
		return CodeBudgetExceeded
	}
	return CodeRateLimitExceeded
}

// Answer is how relay answers a refusal: the HTTP status, error type and code, the class, and the message the caller reads. A usage row records the same status and message.
type Answer struct {
	Status     int
	Type, Code string
	Kind       RefusalKind
	Message    string
	// RetryAfter is the seconds the caller is told to wait; 0 sends no Retry-After.
	RetryAfter int64
}

// RetryAfterSeconds rounds d up to whole seconds, at least 1: an SDK reads "0" as retry at once.
func RetryAfterSeconds(d time.Duration) int64 {
	secs := int64(1)
	if d > 0 {
		secs = int64((d + time.Second - 1) / time.Second)
	}
	return secs
}

// LimitAnswer answers a refused caller counter, naming the rung and policy whose limit refused, never the rule itself.
func LimitAnswer(r *limits.Refused) Answer {
	code := LimitCode(r)
	what := "rate limit"
	if code == CodeBudgetExceeded {
		what = "budget"
	}
	var rung limits.Rung
	var policyID string
	if r.Counter != nil {
		rung, policyID = r.Counter.Rung, r.Counter.PolicyID
	}
	secs := RetryAfterSeconds(r.RetryAfter)
	return Answer{
		Status: http.StatusTooManyRequests, Type: "rate_limit_error", Code: code, Kind: KindOf(code),
		Message:    fmt.Sprintf("the %s of the %s policy %s is exhausted; try again in %ds", what, rung, policyID, secs),
		RetryAfter: secs,
	}
}

// Issue codes the controllers attach besides duplicate_binding and unpriced_under_budget, which they share with refusals.
const (
	IssueRefMissing            = "ref_missing"
	IssueSubjectOutOfScope     = "subject_out_of_scope"
	IssueLinkOutOfScope        = "link_out_of_scope"
	IssueModelRefInvalid       = "model_ref_invalid"
	IssueModelGroupUnsupported = "model_group_unsupported"
	IssueCredentialUnresolved  = "credential_unresolved"
	IssueHostMismatch          = "host_mismatch"
	IssueCredentialsIgnored    = "credentials_ignored"
	IssuePolicyNotBindable     = "policy_not_bindable"
	// IssueSharedAccountMissing is a shared account whose real account is gone, or no longer a provider account at its host: it serves nothing.
	IssueSharedAccountMissing = "shared_account_missing"
)

// rungRefusal is the refusal a rung's policy imposes, naming the rung.
func rungRefusal(code string, r *Rung, policyID, msg string) *Refusal {
	return &Refusal{Code: code, Rung: r.Kind, SubjectID: r.SubjectID, PolicyID: policyID, Message: msg}
}

func refMissing(src engine.Ref, field string, target engine.Ref) engine.Issue {
	return engine.Issue{
		Severity: engine.SeverityWarning,
		Code:     IssueRefMissing,
		Source:   src,
		Target:   target,
		Field:    field,
		Message:  fmt.Sprintf("%s %s does not exist; the link is ignored", target.Kind, target.ID),
	}
}

// parentMissing is ref_missing for a link the row cannot serve without, so the row produces no plan.
func parentMissing(src engine.Ref, field string, target engine.Ref) engine.Issue {
	i := refMissing(src, field, target)
	i.Severity = engine.SeverityError
	i.Message = fmt.Sprintf("%s %s does not exist; %s %s cannot serve", target.Kind, target.ID, src.Kind, src.ID)
	return i
}
