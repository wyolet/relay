package manifest

import (
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/password"
)

// UserSpec is an account; metadata.name is its username. Email, roles and disabled are declarative: one left out is cleared on apply. Password and identity are set outside manifests too (a password change, a first sign-in), so leaving one out keeps the stored value and an explicit null, "" or {} clears it.
type UserSpec struct {
	Email string `yaml:"email,omitempty" json:"email,omitempty"`
	// Password is a bcrypt hash or an environment variable. Nil keeps the stored password; a Password with neither set clears it, ending password login.
	Password *Password `yaml:"password,omitempty" json:"password,omitempty"`
	// Identity is the external identity the account signs in with. Nil keeps the stored one; an Identity with neither field set clears it.
	Identity *Identity `yaml:"identity,omitempty" json:"identity,omitempty"`
	// Roles are role names the account holds directly.
	Roles    []string `yaml:"roles,omitempty"    json:"roles,omitempty"`
	Disabled bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Password is exactly one of a bcrypt hash or an environment variable holding a password or a bcrypt hash. A password read from the environment is hashed on apply.
type Password struct {
	Hash      string     `yaml:"hash,omitempty"      json:"hash,omitempty"`
	ValueFrom *ValueFrom `yaml:"valueFrom,omitempty" json:"valueFrom,omitempty"`
}

// ValueFrom names where a value is read from.
type ValueFrom struct {
	Env string `yaml:"env" json:"env"`
}

// Identity is an identity provider's issuer and subject for the account.
type Identity struct {
	Issuer  string `yaml:"issuer"  json:"issuer"`
	Subject string `yaml:"subject" json:"subject"`
}

// clears reports a Password that clears the stored one.
func (p *Password) clears() bool { return p.Hash == "" && p.ValueFrom == nil }

// clears reports an Identity that clears the stored one.
func (i *Identity) clears() bool { return i.Issuer == "" && i.Subject == "" }

// clear marks key as explicitly emptied in the document.
func (s *UserSpec) clear(key string) {
	switch key {
	case "password":
		s.Password = &Password{}
	case "identity":
		s.Identity = &Identity{}
	}
}

func (p *Password) check() (field string, err error) {
	hasEnv := p.ValueFrom != nil
	switch {
	case p.Hash != "" && hasEnv:
		return "spec.password", errors.New("set password.hash or password.valueFrom, not both")
	case hasEnv && p.ValueFrom.Env == "":
		return "spec.password.valueFrom.env", errors.New("an environment variable name is required")
	case p.Hash != "" && !password.IsBcryptHash(p.Hash):
		return "spec.password.hash", fmt.Errorf("not a bcrypt hash: %w", ErrPlainPassword)
	}
	return "", nil
}

func (s *UserSpec) check() (field string, err error) {
	if s.Password != nil {
		if field, err := s.Password.check(); err != nil {
			return field, err
		}
	}
	if s.Identity != nil && !s.Identity.clears() && (s.Identity.Issuer == "" || s.Identity.Subject == "") {
		return "spec.identity", errors.New("identity needs both issuer and subject")
	}
	return "", nil
}

// UsesEnv reports whether the document reads its password from the environment, for a product that allows that only on some paths.
func (s *UserSpec) UsesEnv() bool { return s.Password != nil && s.Password.ValueFrom != nil }

// ToUser translates a User document. current is the stored account with that username, or nil. An omitted password or identity keeps current's, and a password read from the environment that current's hash already verifies keeps that hash, so re-applying the same document changes nothing. A document clearing the identities of an account it leaves without a password is ErrNoSignIn, so a plan refuses it before anything is written.
func (r *Registry) ToUser(d *Document, current *directory.User) (*directory.User, error) {
	s := d.User
	if s == nil {
		return nil, docError(d, "spec", errors.New("not a User document"))
	}
	u := &directory.User{Username: d.Metadata.Name, Email: s.Email, Roles: s.Roles, Disabled: s.Disabled}
	if current != nil {
		u.ID, u.PasswordHash, u.Identity = current.ID, current.PasswordHash, current.Identity
	}
	if s.Identity != nil {
		u.Identity = directory.Identity{Issuer: s.Identity.Issuer, Subject: s.Identity.Subject}
	}
	if s.Password != nil && s.Password.clears() {
		u.PasswordHash = ""
	} else if s.Password != nil {
		hash, err := r.passwordHash(s.Password, current)
		if err != nil {
			return nil, docError(d, "spec.password", err)
		}
		u.PasswordHash = hash
	}
	if u.PasswordHash == "" && u.Identity.IsZero() && holdsIdentity(current) {
		return nil, docError(d, "spec.identity", ErrNoSignIn)
	}
	return u, nil
}

func holdsIdentity(u *directory.User) bool {
	return u != nil && (!u.Identity.IsZero() || len(u.Identities) > 0)
}

func (r *Registry) passwordHash(p *Password, current *directory.User) (string, error) {
	if p.Hash != "" {
		return p.Hash, nil
	}
	value, ok := r.cfg.LookupEnv(p.ValueFrom.Env)
	if !ok || value == "" {
		return "", fmt.Errorf("environment variable %q is not set", p.ValueFrom.Env)
	}
	if password.IsBcryptHash(value) {
		return value, nil
	}
	if password.IsPlaceholder(value) {
		return "", fmt.Errorf("environment variable %q holds a published placeholder password", p.ValueFrom.Env)
	}
	if current != nil && current.PasswordHash != "" {
		if ok, plain := password.Verify(current.PasswordHash, value); ok && !plain {
			return current.PasswordHash, nil
		}
	}
	return password.Hash(value)
}

// FromUser renders an account. A bcrypt password renders as its hash, so a product exporting accounts decides whether to keep it; a legacy plain-text value is left out rather than printed.
func (r *Registry) FromUser(u *directory.User) *Document {
	s := &UserSpec{Email: u.Email, Roles: u.Roles, Disabled: u.Disabled}
	if password.IsBcryptHash(u.PasswordHash) {
		s.Password = &Password{Hash: u.PasswordHash}
	}
	if !u.Identity.IsZero() {
		s.Identity = &Identity{Issuer: u.Identity.Issuer, Subject: u.Identity.Subject}
	}
	return &Document{APIVersion: APIVersion, Kind: KindUser, Metadata: Metadata{Name: u.Username}, User: s}
}
