// The guards that read a sibling store (policy, team and role refs) reach concrete stores and are exercised by the integration tests; here are the ones that decide on the incoming row alone.

package control

import (
	"context"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/meta"
)

// statusOf is the HTTP status err answers with, 0 for nil.
func statusOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	se, ok := err.(huma.StatusError)
	if !ok {
		t.Fatalf("error %v is not a huma.StatusError", err)
	}
	return se.GetStatus()
}

// A stored credential rotates through its own endpoint so the old hash keeps a grace window; a value on PUT would replace it with no grace and no rotation to audit.
func TestGuardHostKeyRefusesAValueOnUpdate(t *testing.T) {
	const refused = "value cannot be set on update — use POST /host-keys/by-id/{id}/rotate to rotate the credential"
	mk := func(kind hostkey.ValueKind, value string) *hostkey.HostKey {
		k := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "k"}}
		k.Spec.HostID, k.Spec.AccountID = meta.NewID(), meta.NewID()
		k.Spec.ValueFrom = hostkey.ValueFrom{Kind: kind}
		k.Spec.Value = value
		return k
	}
	existing := mk(hostkey.ValueKindStored, "")
	existing.Resolved = "sk-current"

	for _, tc := range []struct {
		name, action string
		existing     *hostkey.HostKey
		incoming     *hostkey.HostKey
		want         string
	}{
		{"create carries the value", "create", nil, mk(hostkey.ValueKindStored, "sk-new"), ""},
		{"update with a new stored value", "update", existing, mk(hostkey.ValueKindStored, "sk-new"), refused},
		{"update with a new oauth blob", "update", existing, mk(hostkey.ValueKindOAuth, "{}"), refused},
		{"update resending the current value", "update", existing, mk(hostkey.ValueKindStored, "sk-current"), ""},
		{"update with no value", "update", existing, mk(hostkey.ValueKindStored, ""), ""},
		{"an env ref carries no value to rotate", "update", existing, mk(hostkey.ValueKindEnv, ""), ""},
		{"an env ref's stray value is not a rotation", "update", existing, mk(hostkey.ValueKindEnv, "sk-new"), ""},
		{"delete ignores the value", "delete", existing, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if err := guardHostKey(Deps{})(context.Background(), tc.action, tc.existing, tc.incoming); err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("guardHostKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// A PUT without a value keeps the stored secret; only an explicit value replaces it.
func TestMergeHostKeyPreserveValue(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		kind                    hostkey.ValueKind
		sent                    string
		wantValue, wantResolved string
	}{
		{"stored: the secret is resupplied for re-encryption", hostkey.ValueKindStored, "", "sk-stored", ""},
		// The resolved value of an oauth key is its access token, not the stored blob, so it only stands in for validation.
		{"oauth: the resolved token stands in", hostkey.ValueKindOAuth, "", "", "sk-stored"},
		{"env: nothing to carry", hostkey.ValueKindEnv, "", "", ""},
		{"stored: an explicit value wins", hostkey.ValueKindStored, "sk-new", "sk-new", ""},
		{"oauth: an explicit value wins", hostkey.ValueKindOAuth, "sk-new", "sk-new", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := &hostkey.HostKey{Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: tc.kind}}, Resolved: "sk-stored"}
			incoming := &hostkey.HostKey{Spec: existing.Spec}
			incoming.Spec.Value = tc.sent
			mergeHostKeyPreserveValue(existing, incoming)
			if incoming.Spec.Value != tc.wantValue || incoming.Resolved != tc.wantResolved {
				t.Fatalf("value %q resolved %q, want %q and %q", incoming.Spec.Value, incoming.Resolved, tc.wantValue, tc.wantResolved)
			}
		})
	}
	mergeHostKeyPreserveValue(nil, &hostkey.HostKey{})
	mergeHostKeyPreserveValue(&hostkey.HostKey{}, nil)

	// An oauth key stored without a token resolves to nothing; the edit still keeps its credential.
	tokenless := &hostkey.HostKey{
		Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: "h", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindOAuth, Provider: "service-account"}},
	}
	edit := &hostkey.HostKey{Meta: tokenless.Meta, Spec: tokenless.Spec}
	mergeHostKeyPreserveValue(tokenless, edit)
	if err := edit.Validate(); err != nil || !edit.KeepStoredValue {
		t.Fatalf("edit of a tokenless oauth key: %v (keeps value %v)", err, edit.KeepStoredValue)
	}

	// A key switching to oauth has no oauth blob to keep, so it must bring a value.
	for _, from := range []hostkey.ValueFrom{{Kind: hostkey.ValueKindEnv, Env: "ACME_KEY"}, {Kind: hostkey.ValueKindStored}} {
		t.Run(string(from.Kind)+" to oauth", func(t *testing.T) {
			existing := &hostkey.HostKey{Meta: tokenless.Meta, Spec: hostkey.Spec{HostID: "h", ValueFrom: from}, Resolved: "sk-existing"}
			switched := &hostkey.HostKey{Meta: tokenless.Meta, Spec: tokenless.Spec}
			mergeHostKeyPreserveValue(existing, switched)
			if err := switched.Validate(); err == nil || switched.KeepStoredValue || switched.Resolved != "" {
				t.Fatalf("switch without a value: err %v, keeps value %v, resolved %q; want it refused", err, switched.KeepStoredValue, switched.Resolved)
			}
		})
	}
}

// Spec is the source of truth for a scoped row's owner: a caller who sends a mismatched owner must not end up with a row filed under it.
func TestGuardsReDeriveTheOwnerFromSpec(t *testing.T) {
	teamID, projectID := meta.NewID(), meta.NewID()
	wrong := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}

	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Owner: wrong}}
	proj.Spec.TeamID = teamID
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "sa", Owner: wrong}}
	sa.Spec.ProjectID = projectID
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "rb", Owner: wrong}}
	rb.Spec.RoleID = meta.NewID()
	rb.Spec.Scope = meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "eng"}}

	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		err   error
		owner meta.Owner
		want  meta.Owner
		valid error
	}{
		{"project takes its team", guardProject(Deps{})(ctx, "create", nil, proj), proj.Meta.Owner, meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, proj.Validate()},
		{"service account takes its project", guardServiceAccount(Deps{})(ctx, "create", nil, sa), sa.Meta.Owner, meta.Owner{Kind: meta.OwnerProject, ID: projectID}, sa.Validate()},
		{"role binding takes its scope", guardRoleBinding(Deps{})(ctx, "create", nil, rb), rb.Meta.Owner, meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, rb.Validate()},
	} {
		if tc.err != nil || tc.owner != tc.want || tc.valid != nil {
			t.Errorf("%s: guard %v, owner %+v (want %+v), then Validate %v", tc.name, tc.err, tc.owner, tc.want, tc.valid)
		}
	}
}

func customRole(name string) *role.Role {
	return &role.Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}},
	}
}

// Built-in roles are the relay's own rows: nobody edits one through CRUD and only an admin deletes one, since boot seeds it again. A custom role takes a license to author and none to delete. The built-in checks come first, so an expired license never masks them.
func TestGuardRole(t *testing.T) {
	licensed := &fakeLicense{info: license.Info{Licensed: true, Features: []string{license.FeatureCustomRoles}}}
	sso := &fakeLicense{info: license.Info{Licensed: true, Features: []string{license.FeatureSSO}}}
	builtin := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "viewer", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	custom := customRole("release-manager")
	const (
		needsLicense = "license_required"
		editBuiltin  = `role "viewer" is built in: update goes through the seed, not generic CRUD`
		dropBuiltin  = `role "viewer" is built in: delete goes through the seed, not generic CRUD`
	)

	for _, tc := range []struct {
		name, who, action  string
		lic                license.Service
		existing, incoming *role.Role
		status             int
		msg                string
	}{
		{"custom role under a license", "alice", "create", licensed, nil, custom, 0, ""},
		{"custom role edited under a license", "alice", "update", licensed, custom, custom, 0, ""},
		{"custom role without a license", "alice", "create", &fakeLicense{}, nil, custom, 403, needsLicense},
		{"custom role under a license for another feature", "alice", "create", sso, nil, custom, 403, needsLicense},
		{"custom role edited without a license", "alice", "update", &fakeLicense{}, custom, custom, 403, needsLicense},
		{"no license service at all", "alice", "create", nil, nil, custom, 403, needsLicense},
		{"custom role deleted without a license", "alice", "delete", nil, custom, nil, 0, ""},
		{"built-in name is reserved", "root", "create", licensed, nil, customRole("admin"), 400, `role name "admin" is reserved for the built-in role`},
		{"built-in name is reserved before the license is asked", "root", "create", nil, nil, customRole("developer"), 400, `role name "developer" is reserved for the built-in role`},
		{"a custom role is not renamed onto a built-in", "root", "update", licensed, custom, customRole("viewer"), 400, `role name "viewer" is reserved for the built-in role`},
		{"admin deletes a built-in", "root", "delete", nil, builtin, nil, 0, ""},
		{"admin token deletes a built-in", "token", "delete", nil, builtin, nil, 0, ""},
		{"a user deletes no built-in", "alice", "delete", licensed, builtin, nil, 403, dropBuiltin},
		{"no actor deletes no built-in", "", "delete", licensed, builtin, nil, 403, dropBuiltin},
		{"an admin edits no built-in", "root", "update", licensed, builtin, builtin, 403, editBuiltin},
		{"a built-in is not renamed away", "root", "update", licensed, builtin, custom, 403, editBuiltin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.who != "" {
				ctx = actor.WithActor(ctx, scopeActors[tc.who])
			}
			err := guardRole(Deps{License: tc.lic})(ctx, tc.action, tc.existing, tc.incoming)
			if got := statusOf(t, err); got != tc.status || (err != nil && err.Error() != tc.msg) {
				t.Fatalf("guard = %d %v, want %d %q", got, err, tc.status, tc.msg)
			}
		})
	}
}
