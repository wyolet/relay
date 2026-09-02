// Package license is the license-gate seam app/ code depends on.
//
// The verifier — the release public key, the signature check, the expiry
// grace — lives in internal/license, which app/ must never import. This
// package holds only what a gate needs: the Checker interface, the summary
// the control plane renders, and the unlicensed default. The composition
// root builds the real implementation and hands it in.
//
// Feature names are the vocabulary of the license file's `features` array.
// Only "sso" is gated today; "custom-roles", "scim", "orgs" and
// "audit-export" are reserved for the milestones that implement them.
package license

import (
	"errors"
	"time"
)

// FeatureSSO gates all IdP-backed control-plane login (OIDC today, SAML
// later). Community deployments keep password login.
const FeatureSSO = "sso"

// ErrRequired is the sentinel every gate returns. Callers match it with
// errors.Is; the HTTP layer maps it to 403 and the settings read path
// degrades the gated section to its defaults rather than failing a boot.
var ErrRequired = errors.New("license_required")

// Checker answers whether a gated feature is licensed.
type Checker interface {
	Has(feature string) bool
}

// Info is the license summary the control plane renders on /version and
// /license. The zero value is a community deployment.
type Info struct {
	Licensed  bool      `json:"licensed"            doc:"False on a community deployment (no license, or one past its grace window)."`
	Customer  string    `json:"customer,omitempty"  doc:"Licensed customer, from the file's sub claim."`
	ExpiresAt time.Time `json:"expiresAt,omitempty" doc:"Expiry, from the file's exp claim."`
	Features  []string  `json:"features,omitempty"  doc:"Gated features this license unlocks."`
	Grace     bool      `json:"grace"               doc:"True while an expired license is inside its 30-day grace window."`
}

// Service is what the control plane receives: the gate, plus the two
// operations the /license endpoints need.
type Service interface {
	Checker

	// Info summarizes the live license.
	Info() Info

	// Set resolves the live license: the RELAY_LICENSE_FILE /
	// RELAY_LICENSE environment wins when set, else value. A present but
	// unusable value returns an error and leaves the previous license in
	// place.
	Set(value string) (Info, error)
}

// Community is the unlicensed gate: every gated feature is off. Used when
// no license is configured and as the safe default wherever a Checker is
// absent.
var Community Checker = community{}

type community struct{}

func (community) Has(string) bool { return false }
