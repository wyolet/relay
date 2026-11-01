package budget

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix() *Budget {
	return &Budget{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-monthly", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: Spec{AmountUSD: "2000"},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(b *Budget)
		want string // "" = valid
	}{
		{name: "amount only"},
		{name: "two fractional digits", edit: func(b *Budget) { b.Spec.AmountUSD = "12.50" }},
		{name: "day period", edit: func(b *Budget) { b.Spec.Period = PeriodDay }},
		{name: "week period", edit: func(b *Budget) { b.Spec.Period = PeriodWeek }},
		{name: "alert mode", edit: func(b *Budget) { b.Spec.Mode = ModeAlert }},
		{name: "system owner", edit: func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }},
		{name: "user owner", edit: func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()} }},
		{name: "project owner", edit: func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()} }},
		{name: "amount required", edit: func(b *Budget) { b.Spec.AmountUSD = "" }, want: "AmountUSD"},
		{name: "three fractional digits", edit: func(b *Budget) { b.Spec.AmountUSD = "1.005" }, want: "budgetamount"},
		{name: "negative amount", edit: func(b *Budget) { b.Spec.AmountUSD = "-5" }, want: "budgetamount"},
		{name: "year period", edit: func(b *Budget) { b.Spec.Period = "year" }, want: "Period"},
		{name: "unknown mode", edit: func(b *Budget) { b.Spec.Mode = "soft" }, want: "Mode"},
		{name: "project owner without id", edit: func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject} }, want: "requires an id"},
		{name: "host owner rejected", edit: func(b *Budget) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()} }, want: "owner.kind must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fix()
			if tc.edit != nil {
				tc.edit(b)
			}
			err := b.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate() = %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

// A budget declared without period or mode must store the same values a later read reports, so apply sees no drift.
func TestDefaultFillsPeriodAndMode(t *testing.T) {
	b := fix()
	b.Default()
	if b.Spec.Period != PeriodMonth || b.Spec.Mode != ModeHard {
		t.Fatalf("defaults = %q/%q, want month/hard", b.Spec.Period, b.Spec.Mode)
	}
	b.Spec.Period, b.Spec.Mode = PeriodDay, ModeAlert
	b.Default()
	if b.Spec.Period != PeriodDay || b.Spec.Mode != ModeAlert {
		t.Fatalf("Default overwrote explicit values: %q/%q", b.Spec.Period, b.Spec.Mode)
	}
	var none *Budget
	none.Default()
}
