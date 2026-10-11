package memstore

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) directory.Stores { return New().Stores() })
}

func TestReadsAreCopies(t *testing.T) {
	ctx := context.Background()
	s := New().Stores()
	u := &directory.User{ID: directory.NewID(), Username: "ada", Roles: []string{"viewer"}}
	if err := s.Users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.Roles[0] = "admin"
	got, _ := s.Users.Get(ctx, u.ID)
	got.Roles[0] = "owner"
	again, _ := s.Users.Get(ctx, u.ID)
	if again.Roles[0] != "viewer" {
		t.Fatalf("stored roles = %v, changed through a caller's slice", again.Roles)
	}

	team := &directory.Team{Meta: directory.Metadata{ID: directory.NewID(), Name: "core", Labels: map[string]string{"a": "1"}}}
	if err := s.Teams.Upsert(ctx, team); err != nil {
		t.Fatal(err)
	}
	team.Meta.Labels["a"] = "2"
	got2, _ := s.Teams.Get(ctx, team.Meta.ID)
	if got2.Meta.Labels["a"] != "1" {
		t.Fatalf("stored labels = %v, changed through a caller's map", got2.Meta.Labels)
	}
}
