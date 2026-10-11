// The inference token's claim set lives here rather than in a caller so the
// minting and verifying sides cannot drift apart.

package crypto

import "strings"

// TokenIssuer is the only `iss` a relay-minted token carries.
const TokenIssuer = "relay"

// TokenClaims is the inference token's payload. Every field is required
// except Grp, which is empty for a user in no groups.
type TokenClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"` // "user:<id>"
	Prj string   `json:"prj"` // project id
	Grp []string `json:"grp,omitempty"`
	Ver int      `json:"ver"` // the user's token version at mint time
	Jti string   `json:"jti"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

// UserID returns the user id the "user:<id>" subject names, or "" when the
// subject is malformed or names another principal kind.
func (c TokenClaims) UserID() string {
	id, ok := strings.CutPrefix(c.Sub, "user:")
	if !ok {
		return ""
	}
	return id
}
