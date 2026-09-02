// Package license verifies relay's offline license file.
//
// A license is a JWT-shaped, EdDSA-signed document produced by the owner's
// signing tool (which lives in the private release repo, never here). This
// repo carries only the release public key and the verifier: there is no
// activation call, no phone-home, no network of any kind. A deployment
// behind an airgap verifies exactly as well as one on the internet.
//
// The claim set:
//
//	{"iss":"wyolet","sub":"<customer>","iat":…,"exp":…,
//	 "deployments":1,"features":["sso",…],"support":"business","jti":"…"}
//
// An absent, malformed, wrongly-signed, or long-expired license is never
// fatal — the process degrades to community (every gated feature off) and
// keeps serving. An expired license stays usable for GraceWindow so a
// renewal gap cannot take a gateway down.
//
// Rotating the public key: generate the new Ed25519 pair with the private
// tooling, replace publicKey below with the base64 (std, padded) encoding
// of the 32-byte public key, and ship it in the next major release — one
// key per major version, and licenses signed with the previous key stop
// verifying at that boundary. The private key never enters this repo; the
// procedure for holding it lives in the private runbook.
//
// This package is composition-root side. app/ never imports it — it
// receives the app/license.Service this package builds.
package license

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	applicense "github.com/wyolet/relay/app/license"
)

// publicKey is the release signing key, base64-encoded (std, padded).
// PLACEHOLDER — replaced by the owner at release time; the zero-length
// value below verifies nothing, so an unreplaced build is community-only.
var publicKey = ""

// GraceWindow is how long an expired license keeps working. A renewal gap
// must degrade slowly and loudly, not at midnight on the expiry date.
const GraceWindow = 30 * 24 * time.Hour

// issuer is the only accepted iss claim.
const issuer = "wyolet"

// License is a verified license file.
type License struct {
	Customer    string
	ExpiresAt   time.Time
	Deployments int
	Features    []string
	Support     string

	// Grace is true when ExpiresAt has passed but the license is still
	// inside GraceWindow.
	Grace bool
}

// Has reports whether feature is unlocked. A nil License is community.
func (l *License) Has(feature string) bool {
	if l == nil {
		return false
	}
	for _, f := range l.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// claims is the on-the-wire payload.
type claims struct {
	Issuer      string   `json:"iss"`
	Subject     string   `json:"sub"`
	IssuedAt    int64    `json:"iat"`
	Expires     int64    `json:"exp"`
	Deployments int      `json:"deployments"`
	Features    []string `json:"features"`
	Support     string   `json:"support"`
	ID          string   `json:"jti"`
}

// Load reads the license from the environment: RELAY_LICENSE_FILE (a path)
// or RELAY_LICENSE (the value inline). Returns (nil, nil) when neither is
// set — a community deployment, which is not an error.
func Load(ctx context.Context) (*License, error) {
	return loadAt(ctx, time.Now)
}

func loadAt(_ context.Context, now func() time.Time) (*License, error) {
	if path := os.Getenv("RELAY_LICENSE_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("RELAY_LICENSE_FILE: %w", err)
		}
		return parseAt(string(raw), now)
	}
	if inline := os.Getenv("RELAY_LICENSE"); inline != "" {
		return parseAt(inline, now)
	}
	return nil, nil
}

// Parse verifies value against the release public key and applies the
// expiry grace. An empty value yields (nil, nil).
func Parse(value string) (*License, error) { return parseAt(value, time.Now) }

func parseAt(value string, now func() time.Time) (*License, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("license: no usable release public key in this build")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("license: malformed (want 3 dot-separated segments)")
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return nil, fmt.Errorf("license: header: %w", err)
	} else if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil, fmt.Errorf("license: header: %w", err)
	}
	if hdr.Alg != "EdDSA" {
		return nil, fmt.Errorf("license: unsupported alg %q", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("license: signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(parts[0]+"."+parts[1]), sig) {
		return nil, fmt.Errorf("license: signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("license: payload: %w", err)
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("license: payload: %w", err)
	}
	if c.Issuer != issuer {
		return nil, fmt.Errorf("license: issuer %q is not %q", c.Issuer, issuer)
	}
	t := now().UTC()
	if c.IssuedAt > 0 && time.Unix(c.IssuedAt, 0).UTC().After(t) {
		return nil, fmt.Errorf("license: not valid until %s", time.Unix(c.IssuedAt, 0).UTC())
	}
	exp := time.Unix(c.Expires, 0).UTC()
	if t.After(exp.Add(GraceWindow)) {
		return nil, fmt.Errorf("license: expired %s, past the %s grace window", exp, GraceWindow)
	}
	return &License{
		Customer:    c.Subject,
		ExpiresAt:   exp,
		Deployments: c.Deployments,
		Features:    c.Features,
		Support:     c.Support,
		Grace:       t.After(exp),
	}, nil
}

// Service is the live license: the gate app/ consults plus the resolution
// the /license endpoints drive. Safe for concurrent use.
type Service struct {
	now func() time.Time
	cur atomic.Pointer[License]
}

var _ applicense.Service = (*Service)(nil)

// New returns a Service holding no license (community). now may be nil,
// which means time.Now; tests inject a clock to exercise expiry.
func New(now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{now: now}
}

// Set resolves the live license: the environment wins when set, else
// value. A present but unverifiable value returns an error and leaves the
// previous license in place, so a bad paste through the admin UI cannot
// disable a working deployment.
func (s *Service) Set(value string) (applicense.Info, error) {
	stored, storedErr := parseAt(value, s.now)
	env, envErr := loadAt(context.Background(), s.now)
	// The environment is the authority whenever it is set; a stored value
	// is verified either way, so a bad paste is reported rather than kept.
	switch {
	case envErr != nil:
		return s.Info(), envErr
	case env != nil:
		s.cur.Store(env)
	case storedErr == nil:
		s.cur.Store(stored)
	}
	if storedErr != nil {
		return s.Info(), storedErr
	}
	return s.Info(), nil
}

// Has reports whether feature is unlocked by the live license.
func (s *Service) Has(feature string) bool { return s.cur.Load().Has(feature) }

// Info summarizes the live license.
func (s *Service) Info() applicense.Info {
	l := s.cur.Load()
	if l == nil {
		return applicense.Info{}
	}
	return applicense.Info{
		Licensed:  true,
		Customer:  l.Customer,
		ExpiresAt: l.ExpiresAt,
		Features:  l.Features,
		Grace:     l.Grace,
	}
}
