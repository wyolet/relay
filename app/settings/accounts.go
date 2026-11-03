package settings

import (
	"encoding/json"
	"fmt"
)

// SectionAccounts is the section key for deployment-wide account settings.
const SectionAccounts = "accounts"

// Personal-account switch values, shared by the section, a group and a user.
const (
	PersonalAccountsOn  = "on"
	PersonalAccountsOff = "off"
)

// Accounts configures the accounts relay keeps for users.
type Accounts struct {
	// PersonalAccounts is the deployment's say on personal accounts; a group's or a user's own value overrides it.
	PersonalAccounts string `json:"personalAccounts" enum:"on,off" doc:"Personal accounts for every user: on (the default) or off. Off closes every personal account and the provider accounts users own personally, so users act only through their teams, projects and service accounts; nothing is deleted. A user's own value, then their groups' (off wins), override it."`
}

// Validate accepts on or off.
func (a *Accounts) Validate() error {
	switch a.PersonalAccounts {
	case PersonalAccountsOn, PersonalAccountsOff:
		return nil
	}
	return fmt.Errorf("accounts: personalAccounts must be on or off, got %q", a.PersonalAccounts)
}

// PersonalAccountsAreOn reports whether the section leaves personal accounts on; a nil section does.
func (a *Accounts) PersonalAccountsAreOn() bool {
	return a == nil || a.PersonalAccounts != PersonalAccountsOff
}

// decodeAccounts unmarshals onto the defaults, so a stored row without the field keeps personal accounts on.
func decodeAccounts(raw []byte) (any, error) {
	v := Accounts{PersonalAccounts: PersonalAccountsOn}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}

func init() {
	Register(Section{
		Name:        SectionAccounts,
		Description: "Accounts relay keeps for users. personalAccounts (on by default) turns every user's personal account on or off; a group's or a user's own personalAccount overrides it. Admin only.",
		Defaults:    func() any { return &Accounts{PersonalAccounts: PersonalAccountsOn} },
		Decode:      decodeAccounts,
	})
}
