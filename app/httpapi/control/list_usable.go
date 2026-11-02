// The usable_by list filter: a picker in a team, project or personal context lists only the rows a row of that context may link, by the same refcheck rule a save applies.

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

const usableByParam = "usable_by"

// usableFilter is one kind's usable_by rule: check answers whether a row of linker kind owned by by may link t.
type usableFilter[T any] struct {
	// linker names the linking row in the parameter's description, such as "a policy".
	linker string
	check  func(ctx context.Context, t *T, by meta.Owner) error
}

func (u *usableFilter[T]) param(plural string) *huma.Param {
	return &huma.Param{
		Name: usableByParam,
		In:   "query",
		Description: fmt.Sprintf("Only the %s %s owned by this scope may link, by the rule a save of that row applies: system, user:<id>, team:<id> or project:<id>.",
			plural, u.linker),
		Schema: &huma.Schema{Type: "string", Pattern: `^(system|(user|team|project):[^:]+)$`},
	}
}

// keep returns the items a row owned by by may link.
func (u *usableFilter[T]) keep(ctx context.Context, items []*T, by meta.Owner) ([]*T, error) {
	out := items[:0:0]
	for _, it := range items {
		err := u.check(ctx, it, by)
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

// takeUsableBy parses usable_by out of q, returning the scope (nil when absent) and q without it, so the filter schema never sees it.
func takeUsableBy(q url.Values) (*meta.Owner, url.Values, error) {
	vals, ok := q[usableByParam]
	if !ok {
		return nil, q, nil
	}
	if len(vals) != 1 {
		return nil, nil, huma.Error400BadRequest(usableByParam + " takes one scope")
	}
	by, err := parseScope(vals[0])
	if err != nil {
		return nil, nil, huma.Error400BadRequest(usableByParam + ": " + err.Error())
	}
	rest := maps.Clone(q)
	delete(rest, usableByParam)
	return &by, rest, nil
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
