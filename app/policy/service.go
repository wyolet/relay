// service.go: runtime orchestrator. Picks a key, resolves the applicable
// RL per (provider, model, host), reserves inbound + upstream buckets,
// and rolls them back on failure. One Service per process.
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

// SnapshotReader is the narrow catalog read surface Service needs.
// *appcatalog.Catalog implements it via a thin adapter.
type SnapshotReader interface {
	Policy(id string) (*Policy, bool)
	RateLimit(id string) (*ratelimit.RateLimit, bool)
}

type Service struct {
	snap     SnapshotReader
	selector *keypool.Selector
	limiter  *pkgratelimit.Limiter
}

func NewService(snap SnapshotReader, sel *keypool.Selector, lim *pkgratelimit.Limiter) *Service {
	return &Service{snap: snap, selector: sel, limiter: lim}
}

type AcquireInput struct {
	Policy   *Policy
	Keys     []*hostkey.HostKey
	Excluded []*hostkey.HostKey
	Model    *model.Model
	Host     *host.Host
	Provider string
}

// Acquisition carries the chosen key + its upstream reservation
// (nil when the tier policy has no applicable RL).
type Acquisition struct {
	Key         *hostkey.HostKey
	Reservation *pkgratelimit.Reservation
}

// ErrSaturated: chosen key is over its upstream cap. Service has
// already recorded the failure; caller appends Key to Excluded and
// retries.
var ErrSaturated = errors.New("policy: upstream key saturated")

// ReserveInbound reserves the inbound policy's RL bucket for this
// request. Returns (nil, nil) when the policy has no applicable RL.
func (s *Service) ReserveInbound(ctx context.Context, pol *Policy, providerSlug, modelSlug, hostSlug string) (*pkgratelimit.Reservation, error) {
	rules := s.rulesFor(pol, providerSlug, modelSlug, hostSlug)
	if len(rules) == 0 || s.limiter == nil {
		return nil, nil
	}
	return s.limiter.Reserve(ctx, pol.Meta.Name, rules)
}

// CommitInbound returns the inbound reservation to the bucket with the
// observed usage. Safe to call with res=nil.
func (s *Service) CommitInbound(ctx context.Context, res *pkgratelimit.Reservation, obs pkgratelimit.Observations) error {
	if res == nil || s.limiter == nil {
		return nil
	}
	return s.limiter.Commit(ctx, res, obs)
}

// Acquire picks one key (excluding in.Excluded) and reserves its
// upstream bucket. On ExceededError, records FailureRateLimitShort and
// returns ErrSaturated (with Acquisition.Key set for logging).
func (s *Service) Acquire(ctx context.Context, in AcquireInput) (*Acquisition, error) {
	if s.selector == nil {
		return nil, fmt.Errorf("policy.Service.Acquire: selector not configured")
	}

	scope, algo := "", keypool.KeySelectionPrioritized
	if in.Policy != nil {
		scope = in.Policy.Meta.Name
		algo = in.Policy.EffectiveKeySelection()
	}
	key, err := s.selector.PickWithExclude(ctx, scope, algo, in.Keys, in.Excluded)
	if err != nil {
		return nil, err
	}

	modelSlug, hostSlug := "", ""
	if in.Model != nil {
		modelSlug = in.Model.Meta.Name
	}
	if in.Host != nil {
		hostSlug = in.Host.Meta.Name
	}
	tier, _ := s.snap.Policy(key.Spec.PolicyID)
	rules := s.rulesFor(tier, in.Provider, modelSlug, hostSlug)
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

// Commit returns the upstream reservation to the bucket.
func (s *Service) Commit(ctx context.Context, acq *Acquisition, obs pkgratelimit.Observations) error {
	if acq == nil || acq.Reservation == nil || s.limiter == nil {
		return nil
	}
	return s.limiter.Commit(ctx, acq.Reservation, obs)
}

// Release rolls back the upstream reservation (zero observations) and
// records the key failure.
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

func (s *Service) RecordSuccess(ctx context.Context, acq *Acquisition) {
	if s.selector == nil || acq == nil || acq.Key == nil {
		return
	}
	s.selector.RecordSuccess(ctx, acq.KeyHash())
}

func (a *Acquisition) KeyHash() string {
	if a == nil || a.Key == nil {
		return ""
	}
	return a.Key.KeyHash
}

// rulesFor resolves pol's applicable RL for the request triple and
// converts it to limiter rules. Returns nil when nothing applies.
func (s *Service) rulesFor(pol *Policy, providerSlug, modelSlug, hostSlug string) []pkgratelimit.Rule {
	if pol == nil || s.snap == nil {
		return nil
	}
	rlID := pol.SelectRateLimitID(providerSlug, modelSlug, hostSlug)
	if rlID == "" {
		return nil
	}
	rl, ok := s.snap.RateLimit(rlID)
	if !ok {
		return nil
	}
	return pol.ResolveRules(rl)
}
