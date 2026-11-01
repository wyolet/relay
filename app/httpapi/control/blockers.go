// A row is not deleted while anything references it. blockers groups the
// references scan's rows by kind and field so a refused delete can say what
// to detach and what to reassign or remove first.

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
)

type blockerItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// blockerGroup is every row of one kind pointing at the target through one
// field. Count includes the Hidden rows the caller may not see; Items lists
// only the visible ones.
type blockerGroup struct {
	Kind       string        `json:"kind"`
	Field      string        `json:"field"`
	Detachable bool          `json:"detachable"`
	Count      int           `json:"count"`
	Hidden     int           `json:"hidden"`
	Items      []blockerItem `json:"items"`
}

// resourceInUseError is the 409 a referenced row's delete returns: the
// usual error envelope plus the blockers beside it.
type resourceInUseError struct {
	httpapi.APIError
	Blockers []blockerGroup `json:"blockers"`
}

// blockers lists what references the kind's row id, grouped. A kind with no
// references scan has no blockers.
func blockers(ctx context.Context, d Deps, kind, id string) ([]blockerGroup, error) {
	scan, ok := referenceScans[kind]
	if !ok {
		return nil, nil
	}
	items, err := scan(ctx, d, id)
	if err != nil {
		return nil, err
	}
	return groupBlockers(ctx, d.Authz, items), nil
}

// groupBlockers counts every row, so a row in a scope the caller can't see
// still blocks, but names only the rows the caller may see. Rows of one
// field split by detachable when a list's last entry can't be let go.
func groupBlockers(ctx context.Context, a authz.Authorizer, items []referenceItem) []blockerGroup {
	sortReferences(items)
	type kindField struct {
		kind, field string
		detachable  bool
	}
	groups := []blockerGroup{}
	at := map[kindField]int{}
	for _, it := range items {
		k := kindField{it.Kind, it.Via, it.detachable}
		i, ok := at[k]
		if !ok {
			i = len(groups)
			at[k] = i
			groups = append(groups, blockerGroup{Kind: it.Kind, Field: it.Via, Detachable: it.detachable, Items: []blockerItem{}})
		}
		g := &groups[i]
		g.Count++
		if visibleTo(ctx, a, it.Kind, it.ID, it.owner) {
			g.Items = append(g.Items, blockerItem{ID: it.ID, Name: it.Name})
		} else {
			g.Hidden++
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Kind != groups[j].Kind {
			return groups[i].Kind < groups[j].Kind
		}
		if groups[i].Field != groups[j].Field {
			return groups[i].Field < groups[j].Field
		}
		return groups[i].Detachable && !groups[j].Detachable
	})
	return groups
}

// droppedLinks is what a delete takes from rows that stay: the groups the answer lists and an audit row per changed row, recorded once the delete has landed.
type droppedLinks struct {
	groups []blockerGroup
	rows   []audit.Row
}

// deleteOutput answers a delete: 204, or 200 listing the links it took.
type deleteOutput struct {
	Body func(huma.Context)
}

// answer records the changed rows and builds the delete's answer; a nil receiver is a delete that took nothing.
func (dl *droppedLinks) answer(ctx context.Context) *deleteOutput {
	if dl == nil {
		return &deleteOutput{Body: func(hc huma.Context) { hc.SetStatus(http.StatusNoContent) }}
	}
	for _, r := range dl.rows {
		audit.RecordExtra(ctx, r)
	}
	return &deleteOutput{Body: func(hc huma.Context) {
		hc.SetHeader("Content-Type", "application/json")
		hc.SetStatus(http.StatusOK)
		_ = json.NewEncoder(hc.BodyWriter()).Encode(struct {
			Detached []blockerGroup `json:"detached"`
		}{dl.groups})
	}}
}

// droppedLinks lists the links a delete of the kind's row takes from rows that stay; nil when it takes none.
func (d Deps) droppedLinks(ctx context.Context, kind, id string) (*droppedLinks, error) {
	scan, ok := droppedLinkScans[kind]
	if !ok {
		return nil, nil
	}
	items, err := scan(ctx, d, id)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	dl := &droppedLinks{groups: groupBlockers(ctx, d.Authz, items)}
	for _, it := range items {
		dl.rows = append(dl.rows, audit.Row{Action: authz.Plural(it.Kind) + ".update", Fields: []string{it.Via}, Resource: d.auditResource(it.Kind, it.ID, it.Name, it.owner)})
	}
	return dl, nil
}

// refuseInUse returns the 409 for deleting a row that is still referenced;
// otherwise the links the delete takes from rows that stay, nil for none.
func (d Deps) refuseInUse(ctx context.Context, kind, id, name string) (*droppedLinks, error) {
	groups, err := blockers(ctx, d, kind, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if len(groups) == 0 {
		dl, err := d.droppedLinks(ctx, kind, id)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return dl, nil
	}
	total := 0
	for _, g := range groups {
		total += g.Count
	}
	return nil, &resourceInUseError{
		APIError: httpapi.APIError{
			Err: httpapi.APIErrorBody{
				Type:    "invalid_request_error",
				Code:    "resource_in_use",
				Message: fmt.Sprintf("%s %q is referenced by %d row(s); see blockers", kind, name, total),
			},
			HTTPStatus: http.StatusConflict,
		},
		Blockers: groups,
	}
}
