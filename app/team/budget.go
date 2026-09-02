// budget.go carries the spend cap shape shared by Team and Project. It
// lives here (rather than in a third package) because Team is the outer
// scope; app/project imports it.
package team

// Budget is a recurring spend cap in USD. Amount is a decimal string so
// money never round-trips through a float.
type Budget struct {
	Amount   string `json:"amount"             yaml:"amount"             validate:"required,budgetamount"`
	Period   string `json:"period,omitempty"   yaml:"period,omitempty"   validate:"omitempty,oneof=month week day"`
	OnExceed string `json:"onExceed,omitempty" yaml:"onExceed,omitempty" validate:"omitempty,oneof=block warn"`
}
