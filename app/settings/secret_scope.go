package settings

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/wyolet/relay/pkg/secret"
)

// ErrSecretRefNotAllowed marks a section value that would resolve a secret
// outside the section's scope or send one without TLS.
var ErrSecretRefNotAllowed = errors.New("secret reference not allowed")

// secretScope is what one section's secret references may resolve: env
// names under envPrefixes and stored ids under storedPrefix ("" refuses
// stored refs). Other kinds are refused.
type secretScope struct {
	envPrefixes  []string
	storedPrefix string
}

// secretScopes lists the sections that resolve a secret and send it to a
// destination the section itself names. Their writer could otherwise read
// any relay credential (master key, admin token, DSNs, host keys) by naming
// it, so each section resolves only secrets provisioned for it, and writing
// it takes an admin.
var secretScopes = map[string]secretScope{
	AuthOIDCSection:       {envPrefixes: []string{"RELAY_OIDC_", "WYOLET_OIDC_"}},
	SectionPayloadLogging: {envPrefixes: []string{"RELAY_PAYLOAD_S3_"}, storedPrefix: SectionPayloadLogging + ":"},
}

// AdminOnly reports whether writing section requires an admin caller.
func AdminOnly(section string) bool {
	_, ok := secretScopes[section]
	return ok
}

func checkSecretRef(section, field string, r secret.Ref) error {
	scope := secretScopes[section]
	switch r.Kind {
	case secret.KindEnv:
		for _, p := range scope.envPrefixes {
			if len(r.Env) > len(p) && strings.HasPrefix(r.Env, p) {
				return nil
			}
		}
		return fmt.Errorf("%s: %s: %w: env name must start with %s",
			section, field, ErrSecretRefNotAllowed, strings.Join(scope.envPrefixes, " or "))
	case secret.KindStored:
		if p := scope.storedPrefix; p != "" && len(r.ID) > len(p) && strings.HasPrefix(r.ID, p) {
			return nil
		}
		if scope.storedPrefix == "" {
			return fmt.Errorf("%s: %s: %w: stored refs are not accepted", section, field, ErrSecretRefNotAllowed)
		}
		return fmt.Errorf("%s: %s: %w: stored id must start with %q", section, field, ErrSecretRefNotAllowed, scope.storedPrefix)
	}
	return fmt.Errorf("%s: %s: %w: kind %q is not accepted", section, field, ErrSecretRefNotAllowed, r.Kind)
}

// plainHTTPAllowed reuses the existing opt-out for a plain-HTTP deployment
// (the session cookie loses Secure under it) rather than adding another flag.
func plainHTTPAllowed() bool { return os.Getenv("RELAY_COOKIE_SECURE") == "false" }

func checkSecretURL(section, field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", section, field, err)
	}
	if u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !plainHTTPAllowed())) {
		return fmt.Errorf("%s: %s: %w: must be an https URL (http needs RELAY_COOKIE_SECURE=false)",
			section, field, ErrSecretRefNotAllowed)
	}
	return nil
}
