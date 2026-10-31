// Package budget is the domain layer for the Budget entity: a recurring spend cap in USD over a calendar period in UTC. Policies link budgets by id.
//
// This package validates one row's shape, fills its defaults, and stores it. Out of scope: counting spend, refusing requests over a cap, and the pricing a capped model needs.
package budget

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Period is the calendar period a budget's amount covers, in UTC.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
)

// Mode decides what reaching the cap does.
type Mode string

const (
	// ModeHard refuses requests once the cap is spent.
	ModeHard Mode = "hard"
	// ModeAlert reports the overrun and lets requests through.
	ModeAlert Mode = "alert"
)

// Defaults for the fields a wire form may omit.
const (
	DefaultPeriod = PeriodMonth
	DefaultMode   = ModeHard
)

// Budget is a named, recurring spend cap.
type Budget struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the cap, its period and its mode.
type Spec struct {
	// AmountUSD is a decimal string with at most two fractional digits, so money never round-trips through a float.
	AmountUSD string `json:"amountUsd"        yaml:"amountUsd"        validate:"required,budgetamount"`
	Period    Period `json:"period,omitempty" yaml:"period,omitempty" validate:"omitempty,oneof=day week month"`
	Mode      Mode   `json:"mode,omitempty"   yaml:"mode,omitempty"   validate:"omitempty,oneof=hard alert"`
}

// Default fills the fields a wire form may omit, so a stored row and the manifest that declared it converge instead of differing by an empty period or mode forever. Nil receiver is a no-op.
func (b *Budget) Default() {
	if b == nil {
		return
	}
	if b.Spec.Period == "" {
		b.Spec.Period = DefaultPeriod
	}
	if b.Spec.Mode == "" {
		b.Spec.Mode = DefaultMode
	}
}

// Validate runs intra-row rules via the shared meta.Validator and requires an owner of kind system, team, project or user; team and project owners carry an id.
func (b *Budget) Validate() error {
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	switch b.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	case meta.OwnerTeam, meta.OwnerProject:
		if b.Meta.Owner.ID == "" {
			return fmt.Errorf("budget %q: owner.kind %q requires an id", b.Meta.Name, b.Meta.Owner.Kind)
		}
	default:
		return fmt.Errorf("budget %q: owner.kind must be system, team, project or user, got %q", b.Meta.Name, b.Meta.Owner.Kind)
	}
	return nil
}
