package plan

import (
	"fmt"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/engine"
)

// Refusal says why a principal, a credential or a route cannot serve: a stable code, and the rung, subject and policy that decided it.
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
)

// Issue codes the controllers attach besides duplicate_binding and unpriced_under_budget, which they share with refusals.
const (
	IssueRefMissing            = "ref_missing"
	IssueSubjectInvalid        = "subject_invalid"
	IssueModelRefInvalid       = "model_ref_invalid"
	IssueModelGroupUnsupported = "model_group_unsupported"
	IssueCredentialUnresolved  = "credential_unresolved"
	IssueHostMismatch          = "host_mismatch"
	IssueCredentialsIgnored    = "credentials_ignored"
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
