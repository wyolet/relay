package settings

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/pkg/secret"
)

func oidcWith(issuer, env string) *AuthOIDC {
	return &AuthOIDC{
		Enabled: true, Issuer: issuer, ClientID: "client-1",
		ClientSecretEnv: env, RedirectURL: "https://relay.example.com/api/auth/oidc/callback",
	}
}

func s3With(useSSL bool, ak, sk secret.Ref) *PayloadLogging {
	return &PayloadLogging{Enabled: true, Backend: "s3", S3: PayloadS3{
		Endpoint: "s3.example.com", Bucket: "bkt", UseSSL: useSSL, AccessKey: ak, SecretKey: sk,
	}}
}

func envRef(name string) secret.Ref { return secret.Ref{Kind: secret.KindEnv, Env: name} }

// A section that sends a secret to a destination it names may resolve only
// secrets provisioned for it, never the relay's own credentials.
func TestSecretRefsStayInSectionScope(t *testing.T) {
	t.Setenv("RELAY_COOKIE_SECURE", "")
	inScopeS3 := envRef("RELAY_PAYLOAD_S3_SECRET_KEY")
	cases := []struct {
		name string
		v    validator
		ok   bool
	}{
		{"oidc: relay-prefixed oidc secret", oidcWith("https://idp.example.com", "RELAY_OIDC_CLIENT_SECRET"), true},
		{"oidc: env overlay secret", oidcWith("https://idp.example.com", "WYOLET_OIDC_CLIENT_SECRET"), true},
		{"oidc: no client secret", oidcWith("https://idp.example.com", ""), true},
		{"oidc: admin token", oidcWith("https://idp.example.com", "RELAY_ADMIN_TOKEN"), false},
		{"oidc: master key", oidcWith("https://idp.example.com", "RELAY_MASTER_KEY"), false},
		{"oidc: database dsn", oidcWith("https://idp.example.com", "RELAY_PG_DSN"), false},
		{"oidc: bare prefix", oidcWith("https://idp.example.com", "RELAY_OIDC_"), false},
		{"oidc: plain-http issuer", oidcWith("http://idp.example.com", "RELAY_OIDC_CLIENT_SECRET"), false},
		{"oidc: disabled still checked", &AuthOIDC{ClientSecretEnv: "RELAY_MASTER_KEY"}, false},
		{"s3: prefixed env refs", s3With(true, envRef("RELAY_PAYLOAD_S3_ACCESS_KEY"), inScopeS3), true},
		{"s3: section-scoped stored ref", s3With(true, secret.Ref{Kind: secret.KindStored, ID: "payload-logging:access-key"}, inScopeS3), true},
		{"s3: ambient credentials", s3With(true, secret.Ref{}, secret.Ref{}), true},
		{"s3: master key env", s3With(true, envRef("RELAY_MASTER_KEY"), inScopeS3), false},
		{"s3: unrelated env", s3With(true, envRef("AWS_SECRET_ACCESS_KEY"), inScopeS3), false},
		{"s3: host key stored id", s3With(true, secret.Ref{Kind: secret.KindStored, ID: "0192f3a0-0000-7000-8000-0000000000aa"}, inScopeS3), false},
		{"s3: external backend", s3With(true, secret.Ref{Kind: secret.KindAWS, Path: "prod/openai"}, inScopeS3), false},
		{"s3: plain-http endpoint", s3With(false, envRef("RELAY_PAYLOAD_S3_ACCESS_KEY"), inScopeS3), false},
		{"s3: disabled still checked", &PayloadLogging{Backend: "s3", S3: PayloadS3{UseSSL: true, AccessKey: envRef("RELAY_ADMIN_TOKEN")}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Validate()
			if tc.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrSecretRefNotAllowed) {
				t.Fatalf("err = %v, want ErrSecretRefNotAllowed", err)
			}
		})
	}
}

// RELAY_COOKIE_SECURE=false already marks a plain-HTTP deployment; only
// then may a secret travel without TLS.
func TestPlainHTTPDestinationNeedsCookieSecureOff(t *testing.T) {
	t.Setenv("RELAY_COOKIE_SECURE", "false")
	if err := oidcWith("http://127.0.0.1:9000", "RELAY_OIDC_CLIENT_SECRET").Validate(); err != nil {
		t.Errorf("oidc: %v", err)
	}
	if err := s3With(false, envRef("RELAY_PAYLOAD_S3_ACCESS_KEY"), secret.Ref{}).Validate(); err != nil {
		t.Errorf("s3: %v", err)
	}
	if err := oidcWith("http://127.0.0.1:9000", "RELAY_ADMIN_TOKEN").Validate(); !errors.Is(err, ErrSecretRefNotAllowed) {
		t.Errorf("dev flag must not widen the env scope, got %v", err)
	}
}

// A row written before the rule existed is read back as the section's
// defaults (disabled) instead of failing every settings read.
func TestOutOfScopeRowDegradesToDefaults(t *testing.T) {
	t.Setenv("RELAY_COOKIE_SECURE", "")
	sec, _ := Lookup(AuthOIDCSection)
	v, err := decodeOrDegrade(sec, []byte(`{"enabled":true,"issuer":"https://idp.example.com","clientId":"c","clientSecretEnv":"RELAY_ADMIN_TOKEN","redirectUrl":"https://r.example.com/cb"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := v.(*AuthOIDC); c.Enabled || c.ClientSecretEnv != "" {
		t.Fatalf("got %+v, want defaults", c)
	}
	if _, err := sec.Decode([]byte(`{"clientSecretEnv":"RELAY_ADMIN_TOKEN"}`)); !errors.Is(err, ErrSecretRefNotAllowed) {
		t.Fatalf("write path must refuse, got %v", err)
	}
}

func TestSecretSectionsAreAdminOnly(t *testing.T) {
	for _, s := range []string{AuthOIDCSection, SectionPayloadLogging} {
		if !AdminOnly(s) {
			t.Errorf("%s must be admin-only", s)
		}
	}
	if AdminOnly(SectionParsing) {
		t.Errorf("%s holds no secret and stays open", SectionParsing)
	}
}
