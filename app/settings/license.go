package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/app/license"
)

// SectionLicense holds the inline license value pasted through the admin
// UI, so a deployment can install a key without a redeploy. The
// RELAY_LICENSE_FILE / RELAY_LICENSE environment still wins when set.
const SectionLicense = "license"

// License is the license settings section.
type License struct {
	// Value is the signed license file, verbatim. Empty is a community
	// deployment.
	Value string `json:"value,omitempty"`
}

// Validate accepts any value: only the verifier in the composition root
// can tell a good license from a bad one, and it runs before this is
// written (PUT /license) rather than here.
func (l *License) Validate() error { return nil }

func init() {
	Register(Section{
		Name: SectionLicense,
		Description: "Inline license file for gated features. Written by PUT /license after verification; " +
			"the RELAY_LICENSE_FILE / RELAY_LICENSE environment wins over it when set.",
		Defaults: func() any { return &License{} },
		Decode:   decodeAndValidate[License, *License],
	})
}

// LicenseFrom reads the typed section from a settings Reader, tolerating
// absent or mistyped values (returns the zero value → community).
func LicenseFrom(r Reader) *License {
	if r == nil {
		return &License{}
	}
	if v, ok := r.Setting(SectionLicense); ok {
		if l, ok := v.(*License); ok {
			return l
		}
	}
	return &License{}
}

// gate is the license Checker license-gated sections consult. Package
// state because Validate() is a method on the section value with nowhere
// to thread a dependency through.
var gate atomic.Pointer[license.Checker]

// SetLicenseGate installs the Checker gated sections consult. Called once
// by the composition root before the first settings decode; until then
// every gated feature reads as unlicensed.
func SetLicenseGate(c license.Checker) { gate.Store(&c) }

// requireLicense reports the gate error for feature, or nil when licensed.
func requireLicense(section, feature string) error {
	if c := gate.Load(); c != nil && *c != nil && (*c).Has(feature) {
		return nil
	}
	return fmt.Errorf("%s: %q requires a license: %w", section, feature, license.ErrRequired)
}

// degraded tracks which sections have already logged their gate error, so
// a boot and every reload after it log once rather than on every read.
var degraded sync.Map

// decodeOrDegrade decodes a stored row, falling back to the section's
// defaults when the only problem is a missing license. A deployment that
// stored an enabled gated section and then lost its license must keep
// booting with that feature off — never refuse to start (the section's
// zero value is the community behaviour). Any other decode error still
// propagates.
func decodeOrDegrade(sec Section, raw []byte) (any, error) {
	v, err := sec.Decode(raw)
	if err == nil || !errors.Is(err, license.ErrRequired) {
		return v, err
	}
	if _, logged := degraded.LoadOrStore(sec.Name, struct{}{}); !logged {
		slog.Warn("settings: section disabled — no license", "section", sec.Name, "err", err)
	}
	return sec.Defaults(), nil
}
