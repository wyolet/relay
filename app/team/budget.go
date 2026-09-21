// budget.go carries the spend cap shape shared by Team and Project. It
// lives here (rather than in a third package) because Team is the outer
// scope; app/project imports it.
package team

// DefaultPeriod is the calendar period a Budget that names none is
// measured over.
const DefaultPeriod = "month"

// Budget is a recurring spend cap in USD. Amount is a decimal string so
// money never round-trips through a float.
type Budget struct {
	Amount   string `json:"amount"             yaml:"amount"             validate:"required,budgetamount"`
	Period   string `json:"period,omitempty"   yaml:"period,omitempty"   validate:"omitempty,oneof=month week day"`
	OnExceed string `json:"onExceed,omitempty" yaml:"onExceed,omitempty" validate:"omitempty,oneof=block warn"`
}

// Default fills the fields a wire form may omit, so a stored row and the
// manifest that declared it converge instead of differing by an empty
// period forever. Nil receiver is a no-op.
func (b *Budget) Default() {
	if b == nil {
		return
	}
	if b.Period == "" {
		b.Period = DefaultPeriod
	}
}
