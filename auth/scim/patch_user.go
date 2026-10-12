package scim

import (
	"encoding/json"

	"github.com/wyolet/relay/auth/directory"
)

// userPatch is a user as PATCH changes it: the fields the store keeps.
type userPatch struct {
	userName, email, externalID string
	active                      bool
}

func newUserPatch(u *directory.User) *userPatch {
	return &userPatch{userName: u.Username, email: u.Email, externalID: u.External.ID, active: !u.Disabled}
}

func (u *userPatch) provisioned() directory.ProvisionedUser {
	return directory.ProvisionedUser{Username: u.userName, Email: u.email, ExternalID: u.externalID}
}

// changedFields lists the SCIM attributes that differ from stored, active excluded.
func (u *userPatch) changedFields(stored *directory.User) []string {
	var out []string
	if u.userName != stored.Username {
		out = append(out, "userName")
	}
	if u.email != stored.Email {
		out = append(out, "emails")
	}
	if u.externalID != stored.External.ID {
		out = append(out, "externalId")
	}
	return out
}

func (u *userPatch) apply(op string, p patchPath, v json.RawMessage) error {
	if p.sub != "" && p.name != "emails" {
		return badRequest(typeInvalidPath, "%s has no sub-attribute %q", p.name, p.sub)
	}
	switch p.name {
	case "username":
		if op == opRemove {
			return badRequest(typeInvalidValue, "userName is required")
		}
		s, err := decodeString(v, "userName")
		if err != nil {
			return err
		}
		if s == "" {
			return badRequest(typeInvalidValue, "userName is required")
		}
		u.userName = s
	case "externalid":
		if op == opRemove {
			u.externalID = ""
			return nil
		}
		s, err := decodeString(v, "externalId")
		if err != nil {
			return err
		}
		u.externalID = s
	case "active":
		if op == opRemove {
			return badRequest(typeInvalidValue, "active cannot be removed")
		}
		b, err := decodeBool(v, "active")
		if err != nil {
			return err
		}
		u.active = b
	case "emails":
		return u.applyEmails(op, p, v)
	case "groups":
		return badRequest(typeMutability, "groups is read-only; change membership through /Groups")
	default:
		return badRequest(typeInvalidPath, "%s cannot be patched", p.name)
	}
	return nil
}

// applyEmails changes the one address the store keeps. A value filter is matched against it as a work, primary address; a filter naming another address the store does not hold changes nothing.
func (u *userPatch) applyEmails(op string, p patchPath, v json.RawMessage) error {
	matches := p.filter == nil
	if p.filter != nil && u.email != "" {
		matches = p.filter.eval(map[string]any{"value": u.email, "type": emailType, "primary": true})
	}
	switch {
	case p.sub != "" && p.sub != "value":
		// type, primary and display are fixed by the one-address store.
		return nil
	case op == opRemove:
		if matches {
			u.email = ""
		}
		return nil
	case !matches && u.email != "":
		return nil
	case p.sub == "value":
		s, err := decodeString(v, "emails.value")
		if err != nil {
			return err
		}
		u.email = s
		return nil
	}
	emails, err := decodeOneOrMany[emailIn](v, "emails")
	if err != nil {
		return err
	}
	if e := primaryEmail(emails); e != "" || op == opReplace {
		u.email = e
	}
	return nil
}
