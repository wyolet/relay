package manifest_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

// Write is the write for a product that plans with its own loader: a create the first time, an update after, through the directory either way.
func TestWriteCreatesThenUpdatesThroughTheDirectory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	docs := e.parse(t, bundle)
	refs := &chain{e: e, ids: map[string]map[string]string{}}
	var rows []any
	for i := range docs {
		d := &docs[i]
		var row any
		var err error
		switch d.Kind {
		case manifest.KindTeam:
			row, err = e.reg.ToTeam(d, refs)
		case manifest.KindProject:
			row, err = e.reg.ToProject(d, refs)
		case manifest.KindServiceAccount:
			row, err = e.reg.ToServiceAccount(d, refs)
		case manifest.KindRole:
			row, err = e.reg.ToRole(d, refs)
		case manifest.KindUser:
			row, err = e.reg.ToUser(d, nil)
		case manifest.KindGroup:
			row, err = e.reg.ToGroup(d, refs)
		case manifest.KindRoleBinding:
			row, err = e.reg.ToRoleBinding(d, refs)
		}
		if err != nil {
			t.Fatalf("%s: %v", d.Kind, err)
		}
		refs.remember(d.Kind, d.Metadata.Name, row)
		if err := manifest.Write(ctx, e.dir, admin, row); err != nil {
			t.Fatalf("write %s: %v", d.Kind, err)
		}
		rows = append(rows, row)
	}
	team := rows[0].(*directory.Team)
	team.Meta.DisplayName = "Renamed"
	for _, row := range rows {
		if err := manifest.Write(ctx, e.dir, admin, row); err != nil {
			t.Fatalf("second write %T: %v", row, err)
		}
	}
	stored, _ := e.store.Stores().Teams.Get(ctx, team.Meta.ID)
	if stored.Meta.DisplayName != "Renamed" {
		t.Errorf("team = %+v", stored.Meta)
	}
	if err := manifest.Write(ctx, e.dir, admin, "not a row"); err == nil {
		t.Error("Write accepted a value that is no row")
	}
	for i := len(docs) - 1; i >= 0; i-- {
		if docs[i].Kind == manifest.KindUser {
			continue
		}
		if err := manifest.Delete(ctx, e.dir, docs[i].Kind, refs.ids[docs[i].Kind][docs[i].Metadata.Name]); err != nil {
			t.Fatalf("delete %s: %v", docs[i].Kind, err)
		}
	}
	if err := manifest.Delete(ctx, e.dir, manifest.KindUser, refs.ids[manifest.KindUser]["bob"]); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Delete(ctx, e.dir, "Policy", "x"); err == nil {
		t.Error("Delete accepted an unknown kind")
	}
}

// chain resolves names to the ids of rows already written in the test, minting an id on first sight like a product's loader would.
type chain struct {
	e   *env
	ids map[string]map[string]string
}

func (c *chain) ID(kind, name string) (string, bool) {
	if id, ok := c.ids[kind][name]; ok {
		return id, true
	}
	return c.e.opts.Refs.ID(kind, name)
}

func (c *chain) remember(kind, name string, row any) {
	id := directory.NewID()
	switch x := row.(type) {
	case *directory.Team:
		x.Meta.ID = id
	case *directory.Project:
		x.Meta.ID = id
	case *directory.ServiceAccount:
		x.Meta.ID = id
	case *directory.Role:
		x.Meta.ID = id
	case *directory.User:
		x.ID = id
	case *directory.Group:
		x.Meta.ID = id
	case *directory.RoleBinding:
		x.Meta.ID = id
	}
	if c.ids[kind] == nil {
		c.ids[kind] = map[string]string{}
	}
	c.ids[kind][name] = id
}
