package settings

import "fmt"

// SectionAudit is the section key for the admin audit log's retention
// window. Read live by the audit emitter's hourly prune, so a change takes
// effect without a restart.
const SectionAudit = "audit"

// Audit configures the admin audit log.
type Audit struct {
	// RetentionDays is how long audit rows are kept. 0 disables pruning —
	// deliberate, for deployments that archive the table externally.
	RetentionDays int `json:"retentionDays"`
}

// Validate is enforced before any write.
func (a *Audit) Validate() error {
	if a.RetentionDays < 0 {
		return fmt.Errorf("audit: retentionDays must be >= 0")
	}
	return nil
}

func init() {
	Register(Section{
		Name:        SectionAudit,
		Description: "Admin audit log retention. retentionDays bounds how long control-plane audit rows are kept (0 = keep forever). Hot-reloaded — the next hourly prune uses the new value.",
		Defaults:    func() any { return &Audit{RetentionDays: 365} },
		Decode:      decodeAndValidate[Audit, *Audit],
	})
}
