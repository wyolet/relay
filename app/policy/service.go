// service.go: runtime orchestrator. Picks a key, resolves the applicable
// RL per (provider, model, host), reserves inbound + upstream buckets,
// and rolls them back on failure. One Service per process.
//
// kv round trips per request: Admit sends the inbound reserve and the first pick's reads in one batch; a tier policy with rules adds its reserve after the pick, and each failover is an Acquire (a pick batch, plus the tier reserve). The success post-flight (Settle) is one batch: the commits that settle something (concurrency or token meters) and, only for a half-open probe or a backoff to reset, the key's success write; often it is empty.

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
// Reads take a context so an implementation can serve the snapshot this
// request was authenticated against rather than the newest one — a reload
// landing mid-request must not change the rules the request is metered by.
type SnapshotReader interface {
	Policy(ctx context.Context, id string) (*Policy, bool)
	RateLimit(ctx context.Context, id string) (*ratelimit.RateLimit, bool)
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

	// writeSuccess: the record the key was picked on changes on a success (keypool.Candidate.SuccessNeedsWrite), so Settle writes it.
	writeSuccess bool
}

// ErrSaturated: chosen key is over its upstream cap. Service has
// already recorded the failure; caller appends Key to Excluded and
// retries.
var ErrSaturated = errors.New("policy: upstream key saturated")

// InboundInput is one request's inbound-reservation context: what to meter
// (the policy plus the resolved triple) and who is asking (the caller's team
// and, for a token, its jti).
type InboundInput struct {
	Policy                            *Policy
	ProviderSlug, ModelSlug, HostSlug string

	// ModelID buckets a per-model RLBinding's counters; ignored for the
	// policy's flat rate limit.
	ModelID string

	// TeamID anchors the reservation's hash tag when the caller has a
	// project; empty keeps the policy-slug tag.
	TeamID string

	// TokenJTI, when set, adds the revocation check to this Reserve — the
	// one kv call the request already makes.
	TokenJTI string

	// ExtraRules are metered in the same Reserve as the policy's rules, under
	// the same hash tag, so a further ceiling costs no extra kv call. Their
	// keys must already carry their own subject.
	ExtraRules []pkgratelimit.Rule
}

// ReserveInbound reserves the inbound policy's RL bucket for this request.
// Returns (nil, nil) when there is nothing to check: no applicable RL, no
// extra rule and no token to check for revocation.
func (s *Service) ReserveInbound(ctx context.Context, in InboundInput) (*pkgratelimit.Reservation, error) {
	rules, metered := s.inboundRules(ctx, in)
	if len(rules) == 0 || s.limiter == nil {
		return nil, nil
	}
	res, err := s.limiter.Reserve(ctx, inboundScope(in), rules)
	if err != nil {
		return nil, err
	}
	if !metered {
		// Revocation check only: nothing was metered, so the post-flight commit would be a second script call for no state.
		res.SetNoCommit()
	}
	return res, nil
}

// inboundRules returns the rules one inbound Reserve checks for in, and whether any of them meters (anything but the revocation check).
func (s *Service) inboundRules(ctx context.Context, in InboundInput) (rules []pkgratelimit.Rule, metered bool) {
	policyRules := s.rulesFor(ctx, in.Policy, in.ProviderSlug, in.ModelSlug, in.HostSlug, in.ModelID)
	nMetered := len(policyRules) + len(in.ExtraRules)
	rules = policyRules
	switch {
	case in.TokenJTI != "" || (len(policyRules) > 0 && len(in.ExtraRules) > 0):
		// The revocation rule goes first: a revoked token must answer 401, not
		// the 429 an over-limit rule evaluated ahead of it would produce.
		// Pre-sized so the rest don't force a regrow.
		rules = make([]pkgratelimit.Rule, 0, nMetered+1)
		if in.TokenJTI != "" {
			rules = append(rules, pkgratelimit.Rule{
				Key:   revokedRuleKey(in.TokenJTI),
				Name:  "token revocation",
				Meter: pkgratelimit.MeterRevoked,
			})
		}
		rules = append(rules, policyRules...)
		rules = append(rules, in.ExtraRules...)
	case len(policyRules) == 0:
		rules = in.ExtraRules
	}
	return rules, nMetered > 0
}

func inboundScope(in InboundInput) string {
	policySlug := ""
	if in.Policy != nil {
		policySlug = in.Policy.Meta.Name
	}
	return reserveScope(in.TeamID, policySlug)
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

	scope, algo := pickScope(in.Policy)
	picked, err := s.selector.PickCandidate(ctx, scope, algo, in.Keys, in.Excluded)
	if err != nil {
		return nil, err
	}
	return s.reserveTier(ctx, picked, in)
}

// pickScope is the key-selection scope and strategy pol sets.
func pickScope(pol *Policy) (string, keypool.KeySelection) {
	if pol == nil {
		return "", keypool.KeySelectionPrioritized
	}
	return pol.Meta.Name, pol.EffectiveKeySelection()
}

// reserveTier reserves the picked key's upstream bucket when its tier policy has rules for this request. On KeyQuotaExhausted it records FailureRateLimitShort and returns ErrSaturated with the key set.
func (s *Service) reserveTier(ctx context.Context, picked keypool.Candidate, in AcquireInput) (*Acquisition, error) {
	key, writeSuccess := picked.Key, picked.SuccessNeedsWrite()
	modelSlug, modelID, hostSlug := "", "", ""
	if in.Model != nil {
		modelSlug = in.Model.Meta.Name
		modelID = in.Model.Meta.ID
	}
	if in.Host != nil {
		hostSlug = in.Host.Meta.Name
	}
	// A miss (tier policy disabled or gone) is unreachable in practice: the
	// tier gate drops such a key before selection. Nil-tolerant rather than
	// fatal; an unmetered request is the failure that gate prevents.
	tier, _ := s.snap.Policy(ctx, key.Spec.PolicyID)
	rules := s.rulesFor(ctx, tier, in.Provider, modelSlug, hostSlug, modelID)
	if len(rules) == 0 || s.limiter == nil {
		return &Acquisition{Key: key, writeSuccess: writeSuccess}, nil
	}

	res, err := s.limiter.Reserve(ctx, "hostkey:"+key.Meta.ID, rules)
	if err != nil {
		var exceeded *pkgratelimit.KeyQuotaExhausted
		if errors.As(err, &exceeded) {
			s.selector.RecordFailure(ctx, key.KeyHash, keypool.FailureRateLimitShort, 0)
			return &Acquisition{Key: key}, ErrSaturated
		}
		return nil, fmt.Errorf("policy.Service.Acquire: reserve: %w", err)
	}
	return &Acquisition{Key: key, Reservation: res, writeSuccess: writeSuccess}, nil
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

func (a *Acquisition) KeyHash() string {
	if a == nil || a.Key == nil {
		return ""
	}
	return a.Key.KeyHash
}

// rulesFor resolves pol's applicable RL for the request triple and
// converts it to limiter rules. Returns nil when nothing applies. modelID
// identifies the request's model so a per-model RLBinding gets its own
// bucket; it is dropped from the key for the policy-wide flat limit.
func (s *Service) rulesFor(ctx context.Context, pol *Policy, providerSlug, modelSlug, hostSlug, modelID string) []pkgratelimit.Rule {
	if pol == nil || s.snap == nil {
		return nil
	}
	rlID, perModel := pol.SelectRateLimitID(providerSlug, modelSlug, hostSlug)
	if rlID == "" {
		return nil
	}
	rl, ok := s.snap.RateLimit(ctx, rlID)
	if !ok {
		return nil
	}
	if !perModel {
		modelID = ""
	}
	return pol.ResolveRules(rl, modelID)
}
