package user

import (
	"strings"

	"github.com/wyolet/relay/auth/directory"
)

// oidcIdentity splits the stored "issuer|sub" at its first separator, as the directory store does; a value without one is a bare subject.
func oidcIdentity(subject string) directory.Identity {
	if subject == "" {
		return directory.Identity{}
	}
	issuer, sub, ok := strings.Cut(subject, directory.IdentitySeparator)
	if !ok {
		return directory.Identity{Subject: subject}
	}
	return directory.Identity{Issuer: issuer, Subject: sub}
}

// ToDirectory is u as the directory stores it.
func (u *User) ToDirectory() *directory.User {
	return &directory.User{
		ID: u.ID, Username: u.Username, Email: u.Email, PasswordHash: u.PasswordHash,
		Identity: oidcIdentity(u.OIDCSubject), Roles: u.Roles, Disabled: u.Disabled,
		TokenVersion: u.TokenVersion, ResourceVersion: u.ResourceVersion, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

// FromDirectory is a directory account as a User; nil stays nil.
func FromDirectory(d *directory.User) *User {
	if d == nil {
		return nil
	}
	u := &User{
		ID: d.ID, Username: d.Username, Email: d.Email, PasswordHash: d.PasswordHash,
		Roles: d.Roles, Disabled: d.Disabled,
		TokenVersion: d.TokenVersion, ResourceVersion: d.ResourceVersion, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
	if !d.Identity.IsZero() {
		u.OIDCSubject = d.Identity.Issuer + directory.IdentitySeparator + d.Identity.Subject
	}
	return u
}
