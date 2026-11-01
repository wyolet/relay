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
	ModelID      string        `json:"modelId"                yaml:"modelId"                validate:"required,uuid" doc:"Id of the model the host serves. One binding per (model, host) pair."`
	HostID       string        `json:"hostId"                 yaml:"hostId"                 validate:"required,uuid" doc:"Id of the host serving the model."`
	Adapter      adapters.Name `json:"adapter"                yaml:"adapter"                doc:"Wire shape relay speaks to the host for this model, such as openai, openai_responses, openai_embeddings, anthropic or gemini."`
	UpstreamName string        `json:"upstreamName,omitempty" yaml:"upstreamName,omitempty" doc:"Model name sent upstream. Empty sends the model's own name."`
	PricingID    string        `json:"pricingId,omitempty"    yaml:"pricingId,omitempty"    validate:"omitempty,uuid" doc:"Id of the pricing that costs this binding's traffic. Empty leaves it unpriced: requests governed by a budget refuse it with unpriced_under_budget."`
	Enabled      *bool         `json:"enabled,omitempty"      yaml:"enabled,omitempty"      doc:"False stops routing the model to this host. Omitted means true."`
	// Snapshots optionally narrows which of the Model's snapshots this Host
	// serves. Empty/nil means "all snapshots"; a non-empty list filters
	// routing to those names only.
	Snapshots []string `json:"snapshots,omitempty" yaml:"snapshots,omitempty" doc:"The model's snapshot names this host serves. Empty serves them all."`
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
//     binding (the dispatch key — see adapters.UpstreamBindingNames).
//   - The owner is never a user, team or project: a binding decides which
//     host a model's traffic goes to, so it is shared catalog data.
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
	if b.Meta.Owner.Tenant() {
		return fmt.Errorf("binding %q: host bindings are shared catalog data: owner.kind cannot be %q", b.Meta.Name, b.Meta.Owner.Kind)
	}
	if !b.Spec.Adapter.UpstreamBinding() {
		return fmt.Errorf("binding %q: adapter %q is not a valid upstream binding (want one of %v)",
			b.Meta.Name, b.Spec.Adapter, adapters.UpstreamBindingNames())
	}
	return nil
}
