package apply

import (
	"context"
	"errors"
	"slices"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// errEnvPassword refuses an environment-sourced password where the environment is the server's and the caller is not its operator.
var errEnvPassword = errors.New("password.valueFrom.env reads the server's environment and is accepted only by `relay seed`; set password.hash instead")

// errLastAdmin is the users endpoint's keep-one-admin rule: with no enabled admin, nobody could grant the role back short of editing the database.
var errLastAdmin = errors.New("this is the last enabled admin; grant the role to another account first")

// planUsers plans User documents. An account has no metadata envelope, so it bypasses planKind: no dirty flag (never skipped), no labels (never pruned), and no owner (authorized at the global scope, as the users endpoints are). Relay's rules for accounts run here: only an admin changes roles or sets a password or identity, and the last enabled admin stays.
func (b *builder) planUsers(ctx context.Context, docs []*authmanifest.Document) error {
	stored := make(map[string]*user.User, len(b.rows.Users))
	admins := map[string]bool{}
	for _, u := range b.rows.Users {
		stored[u.Username] = u
		admins[u.Username] = activeAdmin(u.Disabled, u.Roles)
	}
	hadAdmin := anyAdmin(admins)
	declared := map[string]bool{}
	for _, d := range docs {
		name := d.Metadata.Name
		if declared[name] {
			return &DuplicateError{Kind: authmanifest.KindUser, Name: name}
		}
		declared[name] = true
		if d.User.UsesEnv() && !b.opts.EnvPasswords {
			return &InvalidError{Kind: authmanifest.KindUser, Name: name, Err: errEnvPassword}
		}
		prev := stored[name]
		var current *directory.User
		if prev != nil {
			current = prev.ToDirectory()
		}
		u, err := manifest.AuthManifests.ToUser(d, current)
		if err != nil {
			return &InvalidError{Kind: authmanifest.KindUser, Name: name, Err: err}
		}
		u.ID = b.idx.Users[name]
		if current != nil {
			// The write applies only while the account is as this plan read it.
			u.ResourceVersion = current.ResourceVersion
		}
		e := Entry{Kind: authmanifest.KindUser, Name: name, ID: u.ID, plural: "users", owner: meta.Owner{Kind: meta.OwnerSystem}}
		if d.Metadata.ID != "" && d.Metadata.ID != u.ID {
			e.IDMismatch = d.Metadata.ID
		}
		rolesChanged := len(u.Roles) > 0
		if prev == nil {
			e.Action = ActionCreate
		} else {
			e.prev = rowState{present: true, updatedAt: prev.UpdatedAt}
			rolesChanged = !slices.Equal(prev.Roles, u.Roles)
			fields := authmanifest.ChangedFields(manifest.AuthManifests.FromUser(current), manifest.AuthManifests.FromUser(u))
			e.Action, e.ChangedFields = ActionUpdate, fields
			if len(fields) == 0 {
				e.Action = ActionUnchanged
			}
		}
		if rolesChanged {
			if err := refcheck.RequireAdmin(ctx, b.opts.Authz, "changing roles"); err != nil {
				return &AuthzError{Entry: Entry{Kind: e.Kind, Name: name, plural: e.plural}, Err: err}
			}
		}
		// users.update is not a grant to sign in as the account: a password or identity the caller chose would let it.
		if directory.ChangesCredentials(current, u) {
			if err := refcheck.RequireAdmin(ctx, b.opts.Authz, "setting a password or sign-in identity"); err != nil {
				return &AuthzError{Entry: Entry{Kind: e.Kind, Name: name, plural: e.plural}, Err: err}
			}
		}
		if e.Action != ActionUnchanged {
			row, write := u, throughDirectory(b, infallible(func(x *directory.User) *directory.User { return x }))
			e.write = func(ctx context.Context) error { return write(ctx, row) }
			if b.declared == nil {
				b.declared = map[string]any{}
			}
			b.declared[u.ID] = u
		}
		admins[name] = activeAdmin(u.Disabled, u.Roles)
		b.entries = append(b.entries, e)
	}
	if hadAdmin && !anyAdmin(admins) {
		return &InvalidError{Kind: authmanifest.KindUser, Err: errLastAdmin}
	}
	return nil
}

func activeAdmin(disabled bool, roles []string) bool {
	return !disabled && slices.Contains(roles, user.RoleAdmin)
}

func anyAdmin(admins map[string]bool) bool {
	for _, a := range admins {
		if a {
			return true
		}
	}
	return false
}
