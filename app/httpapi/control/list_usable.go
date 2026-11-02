// The usable_by list filter: a picker in a team, project or personal context lists only the rows a row of that context may link, by the same refcheck rule a save applies. usable_for picks which linking row asks, where a kind is linked by more than one.

package control

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
)

const (
	usableByParam  = "usable_by"
	usableForParam = "usable_for"
)

// usablePurpose is one linking row's rule: check answers whether a row of linker kind owned by by may link t.
type usablePurpose[T any] struct {
	// name is the usable_for value, such as "binding".
	name string
	// linker names the linking row in descriptions, such as "a policy binding".
	linker string
	// check is a refcheck.Checker method, such as refcheck.Checker.UsablePool, run on the request's checker.
	check func(c refcheck.Checker, ctx context.Context, t *T, by meta.Owner) error
}

// usableFilter is one kind's usable_by rules over the checker its saves use; the first purpose is the default.
type usableFilter[T any] struct {
	checker  refcheck.Checker
	purposes []usablePurpose[T]
}

func usableFor[T any](c refcheck.Checker, linker string, check func(refcheck.Checker, context.Context, *T, meta.Owner) error) *usableFilter[T] {
	return &usableFilter[T]{checker: c, purposes: []usablePurpose[T]{{linker: linker, check: check}}}
}

func (u *usableFilter[T]) params(plural string) []*huma.Param {
	def := u.purposes[0]
	by := &huma.Param{
		Name: usableByParam,
		In:   "query",
		Description: fmt.Sprintf("Only the %s %s owned by this scope may link, by the rule a save of that row applies: system, user:<id>, team:<id> or project:<id>.",
			plural, def.linker),
		Schema: &huma.Schema{Type: "string", Pattern: `^(system|(user|team|project):[^:]+)$`},
	}
	if len(u.purposes) == 1 {
		return []*huma.Param{by}
	}
	names := make([]any, len(u.purposes))
	var each []string
	for i, p := range u.purposes {
		names[i] = p.name
		each = append(each, fmt.Sprintf("%s: what %s owned by the usable_by scope may list", p.name, p.linker))
	}
	by.Description = fmt.Sprintf("Only the %s a row owned by this scope may link, by the rule a save of that row applies; usable_for says which row: system, user:<id>, team:<id> or project:<id>.", plural)
	return []*huma.Param{by, {
		Name:        usableForParam,
		In:          "query",
		Description: fmt.Sprintf("Which row usable_by asks for (%s). Default %s. Only with usable_by.", strings.Join(each, "; "), def.name),
		Schema:      &huma.Schema{Type: "string", Enum: names, Default: def.name},
	}}
}

// usableQuery is a parsed usable_by: the scope, the purpose it asks for, and the checker that answers it.
type usableQuery[T any] struct {
	by      meta.Owner
	purpose usablePurpose[T]
	checker refcheck.Checker
}

// keep returns the items a row owned by q.by may link for q's purpose, or the scope's own refusal (refcheck.Checker.UsableScope) when no row could be saved in it. The checker's reads are memoized for this one answer, so checking every row reads each stored row once.
func (q *usableQuery[T]) keep(ctx context.Context, items []*T) ([]*T, error) {
	c := q.checker
	c.Rows = c.Rows.Memoized()
	if err := c.UsableScope(ctx, q.by); err != nil {
		return nil, refErr(err)
	}
	out := items[:0:0]
	for _, it := range items {
		err := q.purpose.check(c, ctx, it, q.by)
		if err == nil {
			out = append(out, it)
			continue
		}
		var re *refcheck.Error
		if !errors.As(err, &re) || re.Status >= http.StatusInternalServerError {
			return nil, huma.Error500InternalServerError(err.Error())
		}
	}
	return out, nil
}

// take parses usable_by and usable_for out of q, returning the query (nil when usable_by is absent) and q without them, so the filter schema never sees them.
func (u *usableFilter[T]) take(q url.Values) (*usableQuery[T], url.Values, error) {
	byVals, hasBy := q[usableByParam]
	forVals, hasFor := q[usableForParam]
	if hasFor && len(u.purposes) == 1 {
		return nil, nil, huma.Error400BadRequest(usableForParam + " is not a filter of this list")
	}
	if !hasBy {
		if hasFor {
			return nil, nil, huma.Error400BadRequest(usableForParam + " applies only with " + usableByParam)
		}
		return nil, q, nil
	}
	if len(byVals) != 1 || len(forVals) > 1 {
		return nil, nil, huma.Error400BadRequest(usableByParam + " and " + usableForParam + " take one value each")
	}
	by, err := parseScope(byVals[0])
	if err != nil {
		return nil, nil, huma.Error400BadRequest(usableByParam + ": " + err.Error())
	}
	out := &usableQuery[T]{by: by, purpose: u.purposes[0], checker: u.checker}
	if hasFor {
		found := false
		for _, p := range u.purposes {
			if p.name == forVals[0] {
				out.purpose, found = p, true
			}
		}
		if !found {
			return nil, nil, huma.Error400BadRequest(fmt.Sprintf("%s: %q is not a purpose of this list", usableForParam, forVals[0]))
		}
	}
	rest := maps.Clone(q)
	delete(rest, usableByParam)
	delete(rest, usableForParam)
	return out, rest, nil
}

// parseScope reads "system", "user:<id>", "team:<id>" or "project:<id>".
func parseScope(s string) (meta.Owner, error) {
	if s == string(meta.OwnerSystem) {
		return meta.Owner{Kind: meta.OwnerSystem}, nil
	}
	kind, id, _ := strings.Cut(s, ":")
	switch k := meta.OwnerKind(kind); k {
	case meta.OwnerUser, meta.OwnerTeam, meta.OwnerProject:
		if id == "" || strings.Contains(id, ":") {
			return meta.Owner{}, fmt.Errorf("%q names no %s id", s, kind)
		}
		return meta.Owner{Kind: k, ID: id}, nil
	}
	return meta.Owner{}, fmt.Errorf("%q is not system, user:<id>, team:<id> or project:<id>", s)
}
