package hostkey

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const someID = "01a19bc3-0000-7000-8000-000000000001"

func fix(kind ValueKind) *HostKey {
	k := &HostKey{
		Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerUser, ID: someID}},
		Spec: Spec{HostID: someID, ValueFrom: ValueFrom{Kind: kind}},
	}
	switch kind {
	case ValueKindEnv:
		k.Spec.ValueFrom.Env = "EXAMPLE_VAR"
	case ValueKindStored:
		k.Spec.Value = "sk-test"
	case ValueKindOAuth:
		k.Spec.Value = `{"access_token":"at","refresh_token":"rt"}`
		k.Spec.ValueFrom.Provider = "claude"
	}
	return k
}

// Owners see the first characters of a long secret; everyone else only a known format prefix and the last 4. Neither shows anything of an unresolved or short secret.
func TestPrefixAndHint(t *testing.T) {
	for _, tc := range []struct{ value, prefix, hint string }{
		{"", "", ""},
		{"sk-short", "", ""},
		{"sk-ant-api03-0123456789a", "sk-ant-a", "sk-ant-...789a"},
		{"sk-ant-api03-0123456789", "", ""},
		{"sk-proj-0123456789abcdefghijkl", "sk-proj-", "sk-proj-...ijkl"},
		{"sk-0123456789abcdefghijklmnop", "sk-01234", "sk-...mnop"},
		{"AIzaSyD0123456789abcdefghijk", "AIzaSyD0", "AIza...hijk"},
		{"f3a91c0d77e24b5c9a1d2e3f4a5b6c7d", "f3a91c0d", "...6c7d"},
	} {
		k := &HostKey{Resolved: tc.value}
		if prefix, hint := k.Prefix(), k.Hint(); prefix != tc.prefix || hint != tc.hint {
			t.Errorf("%q: Prefix %q Hint %q, want %q %q", tc.value, prefix, hint, tc.prefix, tc.hint)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ValueKind
		edit func(k *HostKey)
		want string
	}{
		{"env", ValueKindEnv, func(*HostKey) {}, ""},
		{"stored", ValueKindStored, func(*HostKey) {}, ""},
		{"stored, value already resolved", ValueKindStored, func(k *HostKey) { k.Spec.Value, k.Resolved = "", "sk-x" }, ""},
		{"oauth", ValueKindOAuth, func(*HostKey) {}, ""},
		{"oauth, value already resolved", ValueKindOAuth, func(k *HostKey) { k.Spec.Value, k.Resolved = "", "blob" }, ""},
		{"team-owned", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: someID} }, ""},
		{"system-owned", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }, ""},
		{"organization-owned", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Global }, ""},
		{"organization owner with an id", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerOrganization, ID: someID} }, `hostkey "k": an organization owner carries no id`},
		{"project-owned", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: someID} }, ""},
		{"with account", ValueKindEnv, func(k *HostKey) { k.Spec.AccountID = someID }, ""},
		{"host owner", ValueKindEnv, func(k *HostKey) { k.Meta.Owner.Kind = meta.OwnerHost }, `hostkey "k": owner.kind must be user, organization, system, team or project, got "host"`},
		{"no owner", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{} }, `hostkey "k": owner.kind must be user, organization, system, team or project, got ""`},
		{"team owner without id", ValueKindEnv, func(k *HostKey) { k.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam} }, `hostkey "k": a team owner needs the team's id`},
		{"missing host id", ValueKindEnv, func(k *HostKey) { k.Spec.HostID = "" }, "Key: 'HostKey.Spec.HostID' Error:Field validation for 'HostID' failed on the 'required' tag"},
		{"a host id that is no uuid is left to the host lookup", ValueKindEnv, func(k *HostKey) { k.Spec.HostID = "openai" }, ""},
		{"account not an id", ValueKindEnv, func(k *HostKey) { k.Spec.AccountID = "acme-openai" }, "Key: 'HostKey.Spec.AccountID' Error:Field validation for 'AccountID' failed on the 'uuid' tag"},
		{"env mode missing env", ValueKindEnv, func(k *HostKey) { k.Spec.ValueFrom.Env = "" }, `hostkey "k": valueFrom.env required for env mode`},
		{"env mode with cleartext", ValueKindEnv, func(k *HostKey) { k.Spec.Value = "sk-x" }, `hostkey "k": value must be empty for env mode`},
		{"stored mode missing value", ValueKindStored, func(k *HostKey) { k.Spec.Value = "" }, `hostkey "k": value required for stored mode`},
		{"stored mode with env", ValueKindStored, func(k *HostKey) { k.Spec.ValueFrom.Env = "X" }, `hostkey "k": valueFrom.env must be empty for stored mode`},
		{"oauth mode missing value", ValueKindOAuth, func(k *HostKey) { k.Spec.Value = "" }, `hostkey "k": value (oauth token blob) required for oauth mode`},
		{"oauth mode missing provider", ValueKindOAuth, func(k *HostKey) { k.Spec.ValueFrom.Provider = "" }, `hostkey "k": valueFrom.provider required for oauth mode`},
		{"oauth mode with env", ValueKindOAuth, func(k *HostKey) { k.Spec.ValueFrom.Env = "X" }, `hostkey "k": valueFrom.env must be empty for oauth mode`},
		{"unknown kind", ValueKindEnv, func(k *HostKey) { k.Spec.ValueFrom.Kind = "bogus" }, "Key: 'HostKey.Spec.ValueFrom.Kind' Error:Field validation for 'Kind' failed on the 'oneof' tag"},
		{"no kind", ValueKindEnv, func(k *HostKey) { k.Spec.ValueFrom.Kind = "" }, "Key: 'HostKey.Spec.ValueFrom.Kind' Error:Field validation for 'Kind' failed on the 'required' tag"},
	} {
		k := fix(tc.kind)
		tc.edit(k)
		got := ""
		if err := k.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

// A key's value goes to its host, so an env ref may not name relay's own configuration, and the refusal does not echo the name.
func TestValidateRefusesReservedEnvNames(t *testing.T) {
	const refusal = `hostkey "k": valueFrom.env names a variable reserved for relay's own configuration (RELAY_*, WYOLET_*)`
	for name, reserved := range map[string]bool{
		"RELAY_MASTER_KEY": true, "RELAY_": true, "WYOLET_OIDC_CLIENT_SECRET": true, "relay_master_key": true, "Wyolet_x": true,
		"OPENAI_API_KEY": false, "MY_RELAY_KEY": false, "RELAYER_KEY": false, "RELAY": false, " RELAY_X": false,
	} {
		k := fix(ValueKindEnv)
		k.Spec.ValueFrom.Env = name
		err := k.Validate()
		switch {
		case !reserved && err != nil:
			t.Errorf("%q: Validate() = %v, want it accepted", name, err)
		case reserved && (!errors.Is(err, ErrReservedEnv) || err.Error() != refusal):
			t.Errorf("%q: Validate() = %v, want ErrReservedEnv as %q", name, err, refusal)
		}
	}
}
