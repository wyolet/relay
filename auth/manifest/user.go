package manifest

import (
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/password"
)

// UserSpec is an account; metadata.name is its username. Every field is declarative: one left out is cleared on apply.
type UserSpec struct {
	Email string `yaml:"email,omitempty" json:"email,omitempty"`
	// Password is a bcrypt hash or an environment variable; nil means no password login.
	Password *Password `yaml:"password,omitempty" json:"password,omitempty"`
	// Identity is the external identity the account signs in with.
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

func (p *Password) check() (field string, err error) {
	hasEnv := p.ValueFrom != nil
	switch {
	case p.Hash != "" && hasEnv:
		return "spec.password", errors.New("set password.hash or password.valueFrom, not both")
	case p.Hash == "" && !hasEnv:
		return "spec.password", errors.New("password needs hash or valueFrom.env")
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
	if s.Identity != nil && (s.Identity.Issuer == "" || s.Identity.Subject == "") {
		return "spec.identity", errors.New("identity needs both issuer and subject")
	}
	return "", nil
}

// ToUser translates a User document. current is the stored account with that username, or nil: a password read from the environment that current's hash already verifies keeps that hash, so re-applying the same document changes nothing.
func (r *Registry) ToUser(d *Document, current *directory.User) (*directory.User, error) {
	s := d.User
	if s == nil {
		return nil, docError(d, "spec", errors.New("not a User document"))
	}
	u := &directory.User{Username: d.Metadata.Name, Email: s.Email, Roles: s.Roles, Disabled: s.Disabled}
	if current != nil {
		u.ID = current.ID
	}
	if s.Identity != nil {
		u.Identity = directory.Identity{Issuer: s.Identity.Issuer, Subject: s.Identity.Subject}
	}
	if s.Password == nil {
		return u, nil
	}
	hash, err := r.passwordHash(s.Password, current)
	if err != nil {
		return nil, docError(d, "spec.password", err)
	}
	u.PasswordHash = hash
	return u, nil
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
