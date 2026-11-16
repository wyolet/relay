package group

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

const (
	alice = "01a19bc3-0000-7000-8000-000000000001"
	bob   = "01a19bc3-0000-7000-8000-000000000002"
)

func fix(name string) *Group {
	return &Group{Meta: meta.Metadata{ID: alice, Name: name, Owner: meta.Global}}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(g *Group)
		want string
	}{
		{"organization owner", func(*Group) {}, ""},
		{"user owner", func(g *Group) { g.Meta.Owner.Kind = meta.OwnerUser }, ""},
		{"members", func(g *Group) { g.Spec.MemberIDs = []string{alice, bob} }, ""},
		{"personalAccount on an organization group", func(g *Group) { g.Spec.PersonalAccount = "off" }, ""},
		{"system owner", func(g *Group) { g.Meta.Owner.Kind = meta.OwnerSystem }, ""},
		{"personalAccount on a system group", func(g *Group) { g.Meta.Owner.Kind, g.Spec.PersonalAccount = meta.OwnerSystem, "off" }, ""},
		{"team owner", func(g *Group) { g.Meta.Owner.Kind = meta.OwnerTeam }, `group "data-science": owner.kind must be user or organization, got "team"`},
		{"no owner", func(g *Group) { g.Meta.Owner = meta.Owner{} }, `group "data-science": owner.kind must be user or organization, got ""`},
		{"reserved system: name", func(g *Group) { g.Meta.Name = "system:authenticated" }, `group "system:authenticated": names starting with "system:" are reserved for built-in groups`},
		{"duplicate member", func(g *Group) { g.Spec.MemberIDs = []string{alice, bob, alice} }, `group "data-science": duplicate member id "01a19bc3-0000-7000-8000-000000000001"`},
		{"member id that is no uuid", func(g *Group) { g.Spec.MemberIDs = []string{alice, "bob"} }, "Key: 'Group.Spec.MemberIDs[1]' Error:Field validation for 'MemberIDs[1]' failed on the 'uuid' tag"},
		{"personalAccount outside on and off", func(g *Group) { g.Spec.PersonalAccount = "maybe" }, "Key: 'Group.Spec.PersonalAccount' Error:Field validation for 'PersonalAccount' failed on the 'oneof' tag"},
		{"personalAccount on a user's group", func(g *Group) { g.Meta.Owner.Kind, g.Spec.PersonalAccount = meta.OwnerUser, "on" }, `group "data-science": personalAccount is set only on a group the organization owns; clear it before moving the group to a user, or set it on an organization group`},
	} {
		g := fix("data-science")
		tc.edit(g)
		got := ""
		if err := g.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

// A non-admin's write is compared with the stored group on everything but personalAccount, which only an admin sets.
func TestSameExceptPersonalAccount(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		edit func(g *Group)
		want bool
	}{
		{"untouched", func(*Group) {}, true},
		{"personalAccount", func(g *Group) { g.Spec.PersonalAccount = "off" }, true},
		{"id and description, which no write sets", func(g *Group) { g.Meta.ID, g.Meta.Description = bob, "x" }, true},
		{"name", func(g *Group) { g.Meta.Name = "other" }, false},
		{"display name", func(g *Group) { g.Meta.DisplayName = "Other" }, false},
		{"owner", func(g *Group) { g.Meta.Owner.Kind = meta.OwnerUser }, false},
		{"members", func(g *Group) { g.Spec.MemberIDs = []string{bob, alice} }, false},
		{"enabled", func(g *Group) { g.Spec.Enabled = &off }, false},
	} {
		a, b := fix("eng"), fix("eng")
		a.Spec.MemberIDs, b.Spec.MemberIDs = []string{alice, bob}, []string{alice, bob}
		tc.edit(b)
		if got := SameExceptPersonalAccount(a, b); got != tc.want {
			t.Errorf("%s changed: same = %v, want %v", tc.name, got, tc.want)
		}
	}
	on := true
	a, b := fix("eng"), fix("eng")
	b.Spec.Enabled = &on
	if !SameExceptPersonalAccount(a, b) || !a.IsEnabled() {
		t.Error("an omitted enabled flag and an explicit true differ")
	}
}

// A managed group's source reaches the API read and no write can set it; personalAccount rides in the directory row's attributes.
func TestDirectoryRoundTrip(t *testing.T) {
	g := fix("engineering")
	g.Spec.MemberIDs, g.Spec.PersonalAccount = []string{alice}, "off"
	row := g.ToDirectory()
	if diff := cmp.Diff(map[string]json.RawMessage{"personalAccount": json.RawMessage(`"off"`)}, row.Spec.Attributes); diff != "" {
		t.Errorf("directory attributes (-want +got):\n%s", diff)
	}
	if fix("ops").ToDirectory().Spec.Attributes != nil {
		t.Error("a group without personalAccount carries attributes")
	}
	row.Spec.External = directory.External{Source: "okta", ID: "00g1"}
	back := FromDirectory(row)
	want := fix("engineering")
	want.Spec.MemberIDs, want.Spec.PersonalAccount, want.External = []string{alice}, "off", &External{Source: "okta", ID: "00g1"}
	if diff := cmp.Diff(want, back); diff != "" {
		t.Errorf("FromDirectory (-want +got):\n%s", diff)
	}
	if !back.ToDirectory().Spec.External.IsZero() {
		t.Error("ToDirectory carried the source, so a write could hand a group to one")
	}
	row.Meta.Owner.Kind = directory.OwnerSystem
	if got := FromDirectory(row).Meta.Owner; got != meta.Global {
		t.Errorf("a system-owned row reads as owned by %+v, want the organization", got)
	}

	for name, tc := range map[string]struct {
		g                 *Group
		wantSpec, wantExt string
	}{
		"managed": {back, `{"memberIds":["` + alice + `"],"personalAccount":"off"}`, `{"source":"okta","id":"00g1"}`},
		"local":   {fix("ops"), `{}`, ""},
	} {
		var doc map[string]json.RawMessage
		raw, err := json.Marshal(tc.g)
		if err := errors.Join(err, json.Unmarshal(raw, &doc)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, has := doc["external"]; string(doc["spec"]) != tc.wantSpec || string(doc["external"]) != tc.wantExt || has != (tc.wantExt != "") {
			t.Errorf("%s group JSON = %s\nwant spec %s and external %q beside metadata", name, raw, tc.wantSpec, tc.wantExt)
		}
	}
}
