// Package binding is the domain layer for the HostBinding entity — the
// first-class join that declares "Model M is served by Host H, via wire
// adapter A, under upstream name U, priced by Pricing P."
//
// Bindings used to live embedded in model.Spec.Hosts[]. Promoting them to
// their own entity gives pricing and routing a real row to reference (the
// embedded array was unaddressable), and lets one Model be bound by many
// Hosts — including aggregators re-serving another provider's model —
// without coupling the binding to who owns the Model.
//
// Identity: a binding is a join, owned by no single side, so Owner is
// system-kind. The (ModelID, HostID) pair is unique — enforced at the DB
// (UNIQUE constraint) and re-checked in the catalog composition layer.
package binding

import (
	"fmt"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/meta"
)

// Binding declares how one Host serves one Model.
type Binding struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the (model, host) join plus the per-host serving terms:
// the wire adapter, the upstream model name, an optional Pricing ref, and
// an optional snapshot subset. Cross-refs are stored as ids (the manifest
// layer resolves names↔ids); PricingID is optional.
type Spec struct {
	ModelID      string        `json:"modelId"                yaml:"modelId"                validate:"required,uuid"`
	HostID       string        `json:"hostId"                 yaml:"hostId"                 validate:"required,uuid"`
	Adapter      adapters.Name `json:"adapter"                yaml:"adapter"`
	UpstreamName string        `json:"upstreamName,omitempty" yaml:"upstreamName,omitempty"`
	PricingID    string        `json:"pricingId,omitempty"    yaml:"pricingId,omitempty"    validate:"omitempty,uuid"`
	Enabled      *bool         `json:"enabled,omitempty"      yaml:"enabled,omitempty"`
	// Snapshots optionally narrows which of the Model's snapshots this Host
	// serves. Empty/nil means "all snapshots"; a non-empty list filters
	// routing to those names only.
	Snapshots []string `json:"snapshots,omitempty" yaml:"snapshots,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *Binding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// Serves reports whether this binding is eligible to route requests for the
// given snapshot name. Empty Snapshots means "all"; otherwise membership.
func (b *Binding) Serves(snapshotName string) bool {
	if len(b.Spec.Snapshots) == 0 {
		return true
	}
	for _, s := range b.Spec.Snapshots {
		if s == snapshotName {
			return true
		}
	}
	return false
}

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the binding-specific invariants:
//   - Adapter defaults to OpenAI when omitted, then must be a valid upstream
//     binding (the dispatch key — openai|anthropic|gemini).
//
// Cross-entity checks (ModelID/HostID/PricingID resolve; the (model, host)
// pair is unique; Snapshots name real model snapshots) live in the catalog
// composition + catalogvalidate layers.
func (b *Binding) Validate() error {
	if b.Spec.Adapter == "" {
		b.Spec.Adapter = adapters.DefaultBinding
	}
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	if !b.Spec.Adapter.UpstreamBinding() {
		return fmt.Errorf("binding %q: adapter %q is not a valid upstream binding (want one of %v)",
			b.Meta.Name, b.Spec.Adapter, adapters.UpstreamBindingNames())
	}
	return nil
}
