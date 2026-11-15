package budget

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const someID = "01a19bc3-0000-7000-8000-000000000001"

func fix() *Budget {
	return &Budget{
		Meta: meta.Metadata{ID: someID, Name: "ml-monthly", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: someID}},
		Spec: Spec{AmountUSD: "2000"},
	}
}

// A budget declared without period or mode must store the same values a later read reports, so apply sees no drift.
func TestDefaultFillsPeriodAndMode(t *testing.T) {
	b := fix()
	b.Default()
	if b.Spec.Period != PeriodMonth || b.Spec.Mode != ModeHard {
		t.Errorf("defaults = %q/%q, want month/hard", b.Spec.Period, b.Spec.Mode)
	}
	b.Spec.Period, b.Spec.Mode = PeriodDay, ModeAlert
	b.Default()
	if b.Spec.Period != PeriodDay || b.Spec.Mode != ModeAlert {
		t.Errorf("Default overwrote explicit values: %q/%q", b.Spec.Period, b.Spec.Mode)
	}
	var none *Budget
	none.Default()
}

func TestValidate(t *testing.T) {
	amount := func(v string) func(*Budget) { return func(b *Budget) { b.Spec.AmountUSD = v } }
	owner := func(kind meta.OwnerKind, id string) func(*Budget) {
		return func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: kind, ID: id} }
	}
	const badAmount = "Key: 'Budget.Spec.AmountUSD' Error:Field validation for 'AmountUSD' failed on the 'budgetamount' tag"
	for _, tc := range []struct {
		name string
		edit func(b *Budget)
		want string
	}{
		{"amount only", func(*Budget) {}, ""},
		{"one fractional digit", amount("12.5"), ""},
		{"two fractional digits", amount("12.50"), ""},
		{"zero", amount("0"), ""},
		{"day period", func(b *Budget) { b.Spec.Period = PeriodDay }, ""},
		{"week period", func(b *Budget) { b.Spec.Period = PeriodWeek }, ""},
		{"month period", func(b *Budget) { b.Spec.Period = PeriodMonth }, ""},
		{"alert mode", func(b *Budget) { b.Spec.Mode = ModeAlert }, ""},
		{"hard mode", func(b *Budget) { b.Spec.Mode = ModeHard }, ""},
		{"system owner", owner(meta.OwnerSystem, ""), ""},
		{"organization owner", owner(meta.OwnerOrganization, ""), ""},
		{"user owner", owner(meta.OwnerUser, someID), ""},
		{"project owner", owner(meta.OwnerProject, someID), ""},
		{"no amount", amount(""), "Key: 'Budget.Spec.AmountUSD' Error:Field validation for 'AmountUSD' failed on the 'required' tag"},
		{"three fractional digits", amount("1.005"), badAmount},
		{"negative amount", amount("-5"), badAmount},
		{"explicit plus sign", amount("+5"), badAmount},
		{"no digit before the point", amount(".50"), badAmount},
		{"no digit after the point", amount("5."), badAmount},
		{"exponent", amount("1e3"), badAmount},
		{"thousands separator", amount("2,000"), badAmount},
		{"surrounding space", amount(" 20"), badAmount},
		{"year period", func(b *Budget) { b.Spec.Period = "year" }, "Key: 'Budget.Spec.Period' Error:Field validation for 'Period' failed on the 'oneof' tag"},
		{"unknown mode", func(b *Budget) { b.Spec.Mode = "soft" }, "Key: 'Budget.Spec.Mode' Error:Field validation for 'Mode' failed on the 'oneof' tag"},
		{"project owner without id", owner(meta.OwnerProject, ""), `budget "ml-monthly": owner.kind "project" requires an id`},
		{"team owner without id", owner(meta.OwnerTeam, ""), `budget "ml-monthly": owner.kind "team" requires an id`},
		{"organization owner with an id", owner(meta.OwnerOrganization, someID), `budget "ml-monthly": an organization owner carries no id`},
		{"host owner", owner(meta.OwnerHost, someID), `budget "ml-monthly": owner.kind must be system, organization, team, project or user, got "host"`},
		{"no owner", owner("", ""), `budget "ml-monthly": owner.kind must be system, organization, team, project or user, got ""`},
	} {
		b := fix()
		tc.edit(b)
		got := ""
		if err := b.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
