// service.go: runtime orchestrator. Picks a key, resolves the applicable
// RL per (provider, model, host), reserves inbound + upstream buckets,
// and rolls them back on failure. One Service per process.
//
// kv round trips per request: Admit sends the inbound reserve and the first pick's reads in one batch; a tier policy with rules adds its reserve after the pick, and each failover is an Acquire (a pick batch, plus the tier reserve). A key whose tier reserve was refused is skipped for that model until the refusal's retry-after, without a kv call (tier_full.go). The success post-flight (Settle) is one batch: the commits that settle something (concurrency or token meters) and, only for a half-open probe or a backoff to reset, the key's success write; often it is empty. A failed attempt's Release is one batch (the refund and the key's failure write), and a request ending in an error settles in one more (SettleFailed). All of these run on the post-flight store (WithPostFlight). With the settle queue on (settle_queue.go), the settles of many requests share one batch.

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
	// postSelector and postLimiter run every write after an attempt: Settle, SettleFailed, Release, CommitInbound and Commit.
	postSelector *keypool.Selector
	postLimiter  *pkgratelimit.Limiter
	// settleQueue, when set, coalesces Settle across requests (WithSettleQueue).
	settleQueue *settleQueue

	full  tierFull
	clock func() time.Time
}

func NewService(snap SnapshotReader, sel *keypool.Selector, lim *pkgratelimit.Limiter) *Service {
	return &Service{snap: snap, selector: sel, limiter: lim, postSelector: sel, postLimiter: lim, clock: time.Now}
}

// WithPostFlight runs the writes after an attempt on sel and lim instead, built on a kv store of their own: a served request's commit carries its token counts and a failed one's refund frees its slots, so neither must be dropped when admission has taken every connection of the shared pool. Call before serving.
func (s *Service) WithPostFlight(sel *keypool.Selector, lim *pkgratelimit.Limiter) *Service {
	s.postSelector, s.postLimiter = sel, lim
	return s
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
	// project; empty keeps the policy-id tag.
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
	res, err := s.limiter.ReserveOrShed(ctx, inboundScope(in), rules)
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
	policyID := ""
	if in.Policy != nil {
		policyID = in.Policy.Meta.ID
	}
	return reserveScope(in.TeamID, policyID)
}

// CommitInbound returns the inbound reservation to the bucket with the
// observed usage. Safe to call with res=nil.
func (s *Service) CommitInbound(ctx context.Context, res *pkgratelimit.Reservation, obs pkgratelimit.Observations) error {
	if res == nil || s.postLimiter == nil {
		return nil
	}
	return s.postLimiter.Commit(ctx, res, obs)
}

// Acquire picks one key (excluding in.Excluded and the keys this process
// remembers as over their tier for the model) and reserves its upstream
// bucket. A refused reserve returns ErrSaturated (with Acquisition.Key set so
// the caller can exclude it); no key left but full ones returns ErrKeyTiersFull.
func (s *Service) Acquire(ctx context.Context, in AcquireInput) (*Acquisition, error) {
	if s.selector == nil {
		return nil, fmt.Errorf("policy.Service.Acquire: selector not configured")
	}

	excluded, full := s.full.exclude(in.Keys, in.Excluded, modelIDOf(in), s.clock())
	if full != nil && allExcluded(in.Keys, excluded) {
		return nil, keyTiersFullError(full)
	}
	scope, algo := pickScope(in.Policy)
	picked, err := s.selector.PickCandidate(ctx, scope, algo, in.Keys, excluded)
	if err != nil {
		return nil, pickError(err, full)
	}
	return s.reserveTier(ctx, picked, in)
}

// pickError reports a pick that found no healthy key as ErrKeyTiersFull when
// keys were skipped as full: those have room again at a known time, so the
// caller gets a retry-after instead of "no healthy keys".
func pickError(err error, full *pkgratelimit.KeyQuotaExhausted) error {
	if full != nil && errors.Is(err, keypool.ErrNoHealthyKeys) {
		return keyTiersFullError(full)
	}
	return err
}

func allExcluded(keys, excluded []*hostkey.HostKey) bool {
	skip := make(map[*hostkey.HostKey]struct{}, len(excluded))
	for _, k := range excluded {
		skip[k] = struct{}{}
	}
	for _, k := range keys {
		if _, ok := skip[k]; !ok {
			return false
		}
	}
	return true
}

func modelIDOf(in AcquireInput) string {
	if in.Model == nil {
		return ""
	}
	return in.Model.Meta.ID
}

// pickScope is the key-selection scope and strategy pol sets. The scope is the policy id, so a rename keeps the round-robin and LRU state.
func pickScope(pol *Policy) (string, keypool.KeySelection) {
	if pol == nil {
		return "", keypool.KeySelectionPrioritized
	}
	return pol.Meta.ID, pol.EffectiveKeySelection()
}

// reserveTier reserves the picked key's upstream bucket when its tier policy has rules for this request. On KeyQuotaExhausted it remembers the key as full for the model until the limiter's retry-after and returns ErrSaturated with the key set.
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
			s.full.record(key.Meta.ID, modelID, exceeded, s.clock())
			return &Acquisition{Key: key}, ErrSaturated
		}
		return nil, fmt.Errorf("policy.Service.Acquire: reserve: %w", err)
	}
	return &Acquisition{Key: key, Reservation: res, writeSuccess: writeSuccess}, nil
}

// Commit returns the upstream reservation to the bucket.
func (s *Service) Commit(ctx context.Context, acq *Acquisition, obs pkgratelimit.Observations) error {
	if acq == nil || acq.Reservation == nil || s.postLimiter == nil {
		return nil
	}
	return s.postLimiter.Commit(ctx, acq.Reservation, obs)
}

// releaseTimeout bounds the failure writes (Release, SettleFailed), which ignore the caller's cancellation.
const releaseTimeout = 2 * time.Second

// SettleFailed settles a request that ends in an error: both reservations committed with obs in one round trip, no key write (the attempt loop recorded any failure). Safe with nil reservations or acquisition.
func (s *Service) SettleFailed(ctx context.Context, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) error {
	if s.postLimiter == nil {
		return nil
	}
	var upstream *pkgratelimit.Reservation
	if acq != nil {
		upstream = acq.Reservation
	}
	// The refund must land even when the caller has gone, or the slot stays held until its lease expires.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	return s.postLimiter.CommitBoth(ctx, inbound, upstream, obs)
}

// Release rolls back the upstream reservation (zero observations) and records the key failure, in one round trip.
func (s *Service) Release(ctx context.Context, acq *Acquisition, kind keypool.FailureKind, retryAfter time.Duration) {
	if acq == nil {
		return
	}
	// The refund must land even when the caller has gone, or the slot stays held until its lease expires.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	sel, lim := s.postSelector, s.postLimiter
	refund := acq.Reservation != nil && lim != nil
	if sel == nil {
		if refund {
			_ = lim.Commit(ctx, acq.Reservation, pkgratelimit.Observations{})
		}
		return
	}

	b := sel.NewBatch(2)
	var commit *pkgratelimit.PreparedCommit
	if refund {
		commit, _ = lim.PrepareCommit(b, acq.Reservation, pkgratelimit.Observations{})
	}
	var failure *keypool.PreparedFailure
	if acq.Key != nil {
		failure = sel.PrepareRecordFailure(b, acq.Key.KeyHash, kind, retryAfter)
	}
	b.Exec(ctx)

	if commit != nil {
		_ = commit.Finish()
	}
	switch {
	case failure != nil:
		failure.Finish(ctx)
	case acq.Key != nil:
		sel.RecordFailure(ctx, acq.Key.KeyHash, kind, retryAfter)
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
