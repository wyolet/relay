package settings

// AuthSCIM is the auth:scim settings section: inbound SCIM 2.0 provisioning at /scim/v2 on the control listener. Off by default; while it is off the endpoint answers 404 and SCIM client tokens do nothing.
type AuthSCIM struct {
	Enabled bool `json:"enabled"`
	// DeleteDisables makes a SCIM DELETE of a user disable the account instead of deleting it, keeping its audit trail and bindings.
	DeleteDisables bool `json:"deleteDisables,omitempty"`
}

// Validate has nothing to refuse: both fields are plain switches.
func (a *AuthSCIM) Validate() error { return nil }

// AuthSCIMSection is the section name.
const AuthSCIMSection = "auth:scim"

func init() {
	Register(Section{
		Name: AuthSCIMSection,
		Description: "SCIM 2.0 provisioning from an identity provider (Microsoft Entra ID, Okta) at /scim/v2 on the control listener; disabled by default. " +
			"Each identity provider authenticates with a SCIM client token created under /api/scim-clients. " +
			"deleteDisables makes a SCIM DELETE of a user disable the account instead of deleting it.",
		Defaults: func() any { return &AuthSCIM{} },
		Decode:   decodeAndValidate[AuthSCIM, *AuthSCIM],
	})
}

// AuthSCIMFrom reads the typed section from a settings Reader, tolerating absent or mistyped values (returns the zero value: disabled).
func AuthSCIMFrom(r Reader) *AuthSCIM {
	if r == nil {
		return &AuthSCIM{}
	}
	if v, ok := r.Setting(AuthSCIMSection); ok {
		if c, ok := v.(*AuthSCIM); ok {
			return c
		}
	}
	return &AuthSCIM{}
}
