package settings

// AuthSCIM is the auth:scim settings section: inbound SCIM 2.0 provisioning at /scim/v2 on the control listener. Off by default; while it is off the endpoint answers 404 and SCIM client tokens do nothing.
type AuthSCIM struct {
	Enabled bool `json:"enabled"`
	// DeleteDisables makes a SCIM DELETE of a user disable the account instead of deleting it, keeping its audit trail and bindings.
	DeleteDisables bool `json:"deleteDisables,omitempty"`
	// LinkClaim names the verified id_token claim whose value a first OIDC sign-in matches against a SCIM-provisioned account's externalId; empty means "sub".
	LinkClaim string `json:"linkClaim,omitempty"`
}

// Validate has nothing to refuse: the switches are plain and any claim name may be configured.
func (a *AuthSCIM) Validate() error { return nil }

// EffectiveLinkClaim returns the claim a first sign-in is linked on.
func (a *AuthSCIM) EffectiveLinkClaim() string {
	if a.LinkClaim == "" {
		return "sub"
	}
	return a.LinkClaim
}

// AuthSCIMSection is the section name.
const AuthSCIMSection = "auth:scim"

func init() {
	Register(Section{
		Name: AuthSCIMSection,
		Description: "SCIM 2.0 provisioning from an identity provider (Microsoft Entra ID, Okta) at /scim/v2 on the control listener; disabled by default. " +
			"Each identity provider authenticates with a SCIM client token created under /api/scim-clients. " +
			"deleteDisables makes a SCIM DELETE of a user disable the account instead of deleting it. " +
			"linkClaim names the id_token claim a first OIDC sign-in is linked on: a provisioned account with no sign-in identity whose externalId equals it is signed in (default sub; oid for Entra).",
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
