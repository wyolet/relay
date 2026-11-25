// What an agent removal answers of its rows: those the caller may read through their own read, named; the others, such as a binding a share's revoke left empty in a scope the caller does not see, counted only.

package control

import (
	"context"

	"github.com/wyolet/relay/app/pseudo"
)

// seenRows lists the rows the steps that do op write that the caller may read, in write order (pseudo.Changed), and counts the others.
func (d Deps) seenRows(ctx context.Context, rows []pseudo.Row, op pseudo.Op) ([]CreatedRow, int) {
	out, hidden := []CreatedRow{}, 0
	for _, r := range pseudo.Changed(rows, op) {
		a := d.Authz
		if r.Kind == "account" {
			a = sharerAuthz{Authorizer: d.Authz, d: d}
		}
		if !visibleTo(ctx, a, r.Kind, r.Meta.ID, r.Meta.Owner) {
			hidden++
			continue
		}
		out = append(out, CreatedRow{Kind: r.Kind, ID: r.Meta.ID, Name: r.Meta.Name})
	}
	return out, hidden
}

// keptRows answers the kept rows the caller may see, and counts those it may not.
func keptRows(kept []pseudo.KeptRow) ([]PseudoKept, int) {
	out, hidden := []PseudoKept{}, 0
	for _, k := range kept {
		if !k.Visible {
			hidden++
			continue
		}
		out = append(out, PseudoKept{Kind: k.Kind, ID: k.Meta.ID, Name: k.Meta.Name, Reason: k.Reason})
	}
	return out, hidden
}
