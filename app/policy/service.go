// service.go is the runtime side of policy: the orchestrator that
// composes keypool selection + tier-policy rate-limit resolution +
// limiter reservation into one "acquire/release" lifecycle the
// pipeline can drive.
//
// Layering rationale (see CLAUDE.md and the host-policies design notes):
//
//   - policy.go: data + pure queries (Validate, SelectRateLimitID, etc.).
//     No dependency on keypool/limiter/snapshot. Loadable from PG, safe
//     to pass around as a value.
//
//   - service.go: stateful, per-process orchestrator. Holds Selector,
//     Limiter, and a narrow snapshot reader. Pipeline calls Acquire to
//     pick a key + reserve its upstream rate-limit; on saturation the
//     Service records the failure and signals the caller to retry with
//     a different candidate; on retryable upstream errors the pipeline
//     calls Release to roll back the reservation and tag the key.
//
// One Service per process; methods are goroutine-safe.
package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// SnapshotReader is the narrow read interface Service needs from the
// catalog. Defined here so app/policy doesn't import app/catalog
// directly; appcatalog.Snapshot satisfies this implicitly.
type SnapshotReader interface {
	Policy(id string) (*Policy, bool)
	RateLimit(id string) (*ratelimit.RateLimit, bool)
}

// Service is the runtime application of policies — picks keys, resolves
// per-(key, model) rate-limit rules, reserves upstream budget. Built
// once at boot; safe for concurrent Acquire/Release.
type Service struct {
	snap     SnapshotReader
	selector *keypool.Selector
	limiter  *pkgratelimit.Limiter
}

// NewService wires the dependencies. snap is typically the live
// *appcatalog.Catalog; selector and limiter are the same instances the
// pipeline already uses for the inbound layer.
func NewService(snap SnapshotReader, sel *keypool.Selector, lim *pkgratelimit.Limiter) *Service {
	return &Service{snap: snap, selector: sel, limiter: lim}
}

// AcquireInput is the per-attempt context for Acquire. Provider is the
// owning provider's slug (Meta.Name); routing.Resolver already knows it
// and threads it through.
type AcquireInput struct {
	Policy   *Policy
	Keys     []*hostkey.HostKey
	Excluded []*hostkey.HostKey
	Model    *model.Model
	Host     *host.Host
	Provider string
}

// Acquisition is the outcome of a successful Acquire. Reservation is
// nil when the chosen key's tier policy applies no upstream RL (e.g.
// a placeholder host policy with no caps).
type Acquisition struct {
	Key         *hostkey.HostKey
	Reservation *pkgratelimit.Reservation
}

// ErrSaturated signals that the picked key is over its upstream cap.
// The Service has already recorded a short rate-limit failure on the
// key; the caller's job is to add it to Excluded and call Acquire again
// to try a different candidate. The Acquisition's Key field still
// carries the saturated key for logging context.
var ErrSaturated = errors.New("policy: upstream key saturated")

// Acquire picks one key from in.Keys (excluding in.Excluded) using the
// inbound Policy's KeySelection algo, resolves the key's tier policy
// via Spec.PolicyID, asks that tier which RateLimit applies to this
// (provider, model, host) triple, and reserves the rules under the
// per-key bucket "hostkey:<keyID>".
//
// Errors:
//   - keypool.ErrNoCandidate (or selector's typed variant) when no key
//     can be picked.
//   - ErrSaturated when the picked key's upstream reserve hits
//     ExceededError. The Service has already RecordFailure'd the key;
//     caller appends it to Excluded and retries.
//   - any other error (limiter, snapshot) bubbles unwrapped.
//
// When the tier policy has no RL (or it doesn't apply to this model),
// returns an Acquisition with Reservation=nil and no error — the
// request proceeds with no upstream cap.
func (s *Service) Acquire(ctx context.Context, in AcquireInput) (*Acquisition, error) {
	if s.selector == nil {
		return nil, fmt.Errorf("policy.Service.Acquire: selector not configured")
	}
	if in.Model == nil || in.Host == nil {
		return nil, fmt.Errorf("policy.Service.Acquire: model and host are required")
	}

	scope := ""
	if in.Policy != nil {
		scope = in.Policy.Meta.Name
	}
	key, err := s.selector.PickWithExclude(ctx, scope, in.Policy.EffectiveKeySelection(), in.Keys, in.Excluded)
	if err != nil {
		return nil, err
	}

	rules := s.upstreamRulesFor(key, in.Provider, in.Model.Meta.Name, in.Host.Meta.Name)
	if len(rules) == 0 || s.limiter == nil {
		return &Acquisition{Key: key}, nil
	}

	res, err := s.limiter.Reserve(ctx, "hostkey:"+key.Meta.ID, rules)
	if err != nil {
		var exceeded *pkgratelimit.ExceededError
		if errors.As(err, &exceeded) {
			s.selector.RecordFailure(ctx, key.KeyHash, keypool.FailureRateLimitShort, 0)
			return &Acquisition{Key: key}, ErrSaturated
		}
		return nil, fmt.Errorf("policy.Service.Acquire: reserve: %w", err)
	}
	return &Acquisition{Key: key, Reservation: res}, nil
}

// Commit finalizes an Acquisition after a successful request. Returns
// the upstream reservation to the bucket with the actual observed
// usage. Called from the pipeline's post-flight goroutine. Failures
// are returned for logging; never block the response.
func (s *Service) Commit(ctx context.Context, acq *Acquisition, obs pkgratelimit.Observations) error {
	if acq == nil || acq.Reservation == nil || s.limiter == nil {
		return nil
	}
	return s.limiter.Commit(ctx, acq.Reservation, obs)
}

// Release rolls back a never-committed reservation (zero observations)
// and tags the key with the given failure kind so the breaker
// deprioritizes it on the next pick. Used by the pipeline on retryable
// adapter errors.
func (s *Service) Release(ctx context.Context, acq *Acquisition, kind keypool.FailureKind, retryAfter time.Duration) {
	if acq == nil {
		return
	}
	if acq.Reservation != nil && s.limiter != nil {
		_ = s.limiter.Commit(ctx, acq.Reservation, pkgratelimit.Observations{})
	}
	if s.selector != nil && acq.Key != nil {
		s.selector.RecordFailure(ctx, acq.Key.KeyHash, kind, retryAfter)
	}
}

// RecordSuccess marks a successful request on the key. Mirrors the
// pipeline's existing Selector.RecordSuccess call but funneled through
// Service so callers don't reach into keypool directly.
func (s *Service) RecordSuccess(ctx context.Context, acq *Acquisition) {
	if s.selector == nil || acq == nil || acq.Key == nil {
		return
	}
	s.selector.RecordSuccess(ctx, acq.KeyHash())
}

// KeyHash returns the chosen key's hash for telemetry. Convenience to
// keep callers from poking into Acquisition.Key.KeyHash directly.
func (a *Acquisition) KeyHash() string {
	if a == nil || a.Key == nil {
		return ""
	}
	return a.Key.KeyHash
}

// upstreamRulesFor resolves the key's tier policy → applicable RL →
// pkgratelimit.Rule list. Returns nil if the key has no tier policy,
// the tier doesn't resolve, the tier has no applicable RL, or the RL
// has no rules.
func (s *Service) upstreamRulesFor(k *hostkey.HostKey, providerSlug, modelSlug, hostSlug string) []pkgratelimit.Rule {
	if k == nil || k.Spec.PolicyID == "" || s.snap == nil {
		return nil
	}
	tier, ok := s.snap.Policy(k.Spec.PolicyID)
	if !ok {
		return nil
	}
	rlID := tier.SelectRateLimitID(providerSlug, modelSlug, hostSlug)
	if rlID == "" {
		return nil
	}
	rl, ok := s.snap.RateLimit(rlID)
	if !ok {
		return nil
	}
	return tier.ResolveRules(rl)
}
