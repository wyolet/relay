package team

import (
	"encoding/json"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

// budgetKey is the spec field the budget is stored under, as it was before the directory held these rows.
const budgetKey = "budget"

// BudgetAttribute reads the budget out of a directory row's attributes; nil when the row has none.
func BudgetAttribute(attrs map[string]json.RawMessage) (*Budget, error) {
	var b *Budget
	if err := meta.Attribute(attrs, budgetKey, &b); err != nil {
		return nil, err
	}
	return b, nil
}

// WithBudget returns attrs carrying b; a nil b leaves the budget out.
func WithBudget(attrs map[string]json.RawMessage, b *Budget) (map[string]json.RawMessage, error) {
	return meta.PutAttribute(attrs, budgetKey, b, b != nil)
}

// ToDirectory is t as the directory stores it.
func (t *Team) ToDirectory() (*directory.Team, error) {
	attrs, err := WithBudget(nil, t.Spec.Budget)
	if err != nil {
		return nil, err
	}
	return &directory.Team{
		Meta: meta.ToDirectory(t.Meta),
		Spec: directory.TeamSpec{Enabled: t.Spec.Enabled, Attributes: attrs},
	}, nil
}

// FromDirectory is a directory row as a Team.
func FromDirectory(r *directory.Team) (*Team, error) {
	b, err := BudgetAttribute(r.Spec.Attributes)
	if err != nil {
		return nil, err
	}
	return &Team{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{Enabled: r.Spec.Enabled, Budget: b},
	}, nil
}
