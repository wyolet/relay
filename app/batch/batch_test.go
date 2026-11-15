package batch

import (
	"testing"

	"github.com/wyolet/relay/app/httpapi/inference"
)

func TestCaller_TokenJTIAndOwner(t *testing.T) {
	attr := func(principalKind, principalID, credKind, credID string) Attribution {
		return Attribution{PrincipalKind: principalKind, PrincipalID: principalID, CredentialKind: credKind, CredentialID: credID}
	}
	for _, tc := range []struct {
		name      string
		c         *Caller
		jti, owns string
	}{
		{"nil caller", nil, "", ""},
		{"token", &Caller{Attribution: attr("user", "u1", inference.CredentialToken, "jti-1")}, "jti-1", "user:u1"},
		{"key", &Caller{KeyHash: "h", Attribution: attr("user", "u1", inference.CredentialKey, "key-id")}, "", "h"},
		{"service account key", &Caller{KeyHash: "h2", Attribution: attr("service_account", "sa1", inference.CredentialKey, "k2")}, "", "h2"},
	} {
		if got := tc.c.TokenJTI(); got != tc.jti {
			t.Errorf("%s: TokenJTI = %q, want %q", tc.name, got, tc.jti)
		}
		if got := tc.c.Owner(); got != tc.owns {
			t.Errorf("%s: Owner = %q, want %q", tc.name, got, tc.owns)
		}
	}
}
