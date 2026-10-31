package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// attempt is one credential called (or about to be) with the upstream counters it holds.
type attempt struct {
	hop  *plan.RouteHop
	slot *plan.KeySlot
	cand keypool.Candidate
	res  limits.Reservation
	// reached is set once a call may have reached the upstream: its charge then stands.
	reached bool
}

// runState is what one Run holds: the caller's reservation and the attempt in flight.
type runState struct {
	caller   limits.Reservation
	callerCS []limits.Counter
	// callerReached is set once any attempt under the caller's hold may have reached the upstream.
	callerReached bool
	// cur points at att while an attempt is held; one allocation serves every attempt of the request.
	cur *attempt
	att attempt
	// host is the last host called, for the reachability record of a failure.
	host *host.Host
}

// Run orchestrates one request over req.Hops. Caller MUST Close the returned Result.Body to release the connection and trigger post-flight.
//
// A refusal on a caller counter ends the request; one on an upstream counter skips that credential (cooling its account when the account's counter refused) and the walk goes on. Reservations never reaching the upstream are released, ones that reached it and failed are settled with no usage, and the served one is settled in post-flight with the real tokens and cost.
func (p *Pipeline) Run(ctx context.Context, req *Request) (res *Result, err error) {
	st := &runState{}
	defer func() {
		if err != nil {
			// Settled before the error returns, so a caller retrying at once never finds its own hold.
			st.abandon()
			go p.fireFailure(req, st.host, err)
		}
	}()

	if req.Adapter == nil {
		return nil, ErrAdapterMissing
	}
	slots := 0
	for i := range req.Hops {
		slots += len(req.Hops[i].Slots)
	}
	if slots == 0 {
		return nil, ErrNoKeys
	}
	if p.Limiter == nil {
		return nil, ErrLimiterMissing
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	maxAttempts = min(maxAttempts, slots)

	var (
		attempts int // credentials called upstream (a same-credential retry doesn't count)
		limited  *UpstreamLimitedError
		pickErr  error
		// Last upstream response observed during retry, carried into the final error: without it "all keys exhausted" hides both the reason upstream rejected (auth? quota? bad model?) and the backoff/rate-limit headers the caller needs to pace its retry.
		lastStatus int
		lastHeader http.Header
		lastBody   []byte
	)
hops:
	for hi := range req.Hops {
		hop := &req.Hops[hi]
		if len(hop.Slots) == 0 {
			continue
		}
		if err = p.holdCaller(ctx, st, hop); err != nil {
			return nil, err
		}
		// Most hops hold a few credentials: their exclusions fit a stack buffer.
		var exBuf [8]bool
		excluded := exBuf[:0]
		if n := len(hop.Slots); n <= len(exBuf) {
			excluded = exBuf[:n]
		} else {
			excluded = make([]bool, n)
		}
		for attempts < maxAttempts {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			i, cand, perr := p.pick(ctx, hop, excluded, &limited)
			if i < 0 {
				if perr != nil {
					pickErr = perr
				}
				continue hops
			}
			slot := hop.Slots[i]
			upRes, aerr := p.Limiter.Admit(ctx, hop.UpstreamCounters(i), limits.Usage{})
			if aerr != nil {
				var refused *limits.Refused
				if !errors.As(aerr, &refused) {
					return nil, aerr // a limit that cannot be checked refuses
				}
				// Upstream counters only: the caller's were checked by holdCaller.
				coolAccount(slot, refused)
				limited = noteLimited(limited, refused.RetryAfter, refused)
				excluded[i] = true
				continue
			}
			attempts++
			st.att = attempt{hop: hop, slot: slot, cand: cand, res: upRes}
			st.cur = &st.att
			st.host = hop.Host
			stampAttempt(req.Lifecycle, st.cur, attempts)

			canFailOver := attempts < maxAttempts && (untried(excluded, i) || slotsAfter(req.Hops, hi))
			action, rerr := p.callSlot(ctx, req, st, hop, i, canFailOver)
			if rerr.result != nil {
				return rerr.result, nil
			}
			if rerr.status != 0 {
				lastStatus, lastHeader, lastBody = rerr.status, rerr.header, rerr.body
			}
			switch action {
			case actNextKey:
				st.closeAttempt()
				excluded[i] = true
			case actNextHop:
				// Released before moving on, not detached: when no hop is left the error returns next, and a caller retrying at once must not meet its own hold.
				st.cur.close()
				st.cur = nil
				err = rerr.err
				continue hops
			default: // actStop
				err = rerr.err
				break hops
			}
		}
		break
	}

	if err == nil && attempts == 0 {
		switch {
		case limited != nil:
			return nil, limited
		case pickErr != nil:
			return nil, pickErr
		}
	}
	if err == nil {
		err = ErrAllKeysExhausted
	}
	if errors.Is(err, ErrAllKeysExhausted) && lastStatus != 0 {
		err = &UpstreamFailureError{Status: lastStatus, Header: lastHeader, Body: lastBody, Cause: err}
	}
	return nil, err
}

type attemptAction int

const (
	actNextKey attemptAction = iota // fail over to another credential of the hop
	actNextHop                      // the hop's host is unreachable: go on to the next hop
	actStop                         // stop; surface the upstream error
)

// callOutcome is what calling one slot ended with: a result to return, or the error and last upstream response to carry on.
type callOutcome struct {
	result *Result
	err    error
	status int
	header http.Header
	body   []byte
}

// callSlot calls the attempt's credential until it serves, fails over or stops: a dial failure re-dials the same host with a short backoff (the host's problem, not the key's), and a healed secret retries the same credential.
func (p *Pipeline) callSlot(ctx context.Context, req *Request, st *runState, hop *plan.RouteHop, i int, canFailOver bool) (attemptAction, callOutcome) {
	slot := hop.Slots[i]
	keyValue := slot.Credential.Resolved
	// An OAuth-kind credential authenticates differently upstream; the adapter picks the auth variant per credential, so a failover between kinds authenticates each correctly.
	oauth := slot.Credential.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth
	connAttempts := 0
	var out callOutcome
	for {
		req.Lifecycle.MarkUpstreamStart()
		call := newUpstreamCall(ctx, callTimeout(req.Adapter, req.Stream))
		resp, err := req.Adapter.Call(call.ctx, hop.Host.Spec.BaseURL, hop.Host.Spec.Path, keyValue, req.Body, req.Headers, req.UpstreamModel, req.Stream, oauth)
		if err == nil && resp != nil && !shouldRetry(req.Adapter, resp) {
			call.accepted.Store(true)
			st.cur.reached, st.callerReached = true, true
			out.result = p.makeResult(req, st, resp, call)
			return actStop, out
		}
		// A caller leaving mid-call can surface as a broken pipe or a reset instead of the context error; either way the key did nothing wrong.
		if cerr := ctx.Err(); err != nil && cerr != nil && !errors.Is(err, cerr) {
			err = fmt.Errorf("%w: %w", cerr, err)
		}
		retry, kind, retryAfter := classify(req.Adapter, resp, err)
		if kind != keypool.FailureUpstreamUnreachable {
			st.cur.reached, st.callerReached = true, true
		}
		if resp != nil {
			out.status = resp.StatusCode
			out.header = resp.Header.Clone()
			out.body = readBodyCapped(resp, maxUpstreamErrorBody)
		}
		call.release()
		out.err = err

		if kind == keypool.FailureUpstreamUnreachable {
			connAttempts++
			if connAttempts < maxConnAttempts {
				if werr := sleepCtx(ctx, connBackoff(connAttempts)); werr != nil {
					out.err = werr
					return actStop, out
				}
				continue // same credential and reservation, same host
			}
			out.err = &UpstreamUnreachableError{Host: hop.Host.Meta.Name, Attempts: connAttempts, Cause: err}
			return actNextHop, out
		}

		action, fresh := p.handleFailure(ctx, st.cur, retry, kind, retryAfter, retry && canFailOver)
		if action == actRetrySame {
			keyValue = fresh
			continue
		}
		if action == actStop {
			out.err = err
		}
		return action, out
	}
}

// actRetrySame retries the same credential with a healed secret; only handleFailure returns it.
const actRetrySame attemptAction = -1

// handleFailure decides what to do after a failed call. Non-retryable failures stop without touching the breaker (a 400 isn't the key's fault). For retryable ones it consults the KeyAgent when set; the breaker write is issued on every path that abandons the credential and skipped on a heal-retry. The write runs detached: the next attempt excludes the credential itself, so it never waits on it.
func (p *Pipeline) handleFailure(ctx context.Context, a *attempt, retry bool, kind keypool.FailureKind, retryAfter time.Duration, more bool) (attemptAction, string) {
	if !retry {
		return actStop, ""
	}
	if p.KeyAgent != nil {
		switch v, fresh := p.KeyAgent.OnFailure(ctx, a.slot.Credential.Meta.ID, kind, more); v {
		case keypool.VerdictRetry:
			return actRetrySame, fresh
		case keypool.VerdictFail:
			go p.recordFailure(ctx, a.slot, kind, retryAfter)
			return actStop, ""
		}
	}
	go p.recordFailure(ctx, a.slot, kind, retryAfter)
	return actNextKey, ""
}

func (p *Pipeline) recordFailure(ctx context.Context, s *plan.KeySlot, kind keypool.FailureKind, retryAfter time.Duration) {
	if sel := p.postSelector(); sel != nil {
		sel.RecordFailure(ctx, s.Credential.KeyHash, kind, retryAfter)
	}
}

func (p *Pipeline) postSelector() *keypool.Selector {
	if p.PostSelector != nil {
		return p.PostSelector
	}
	return p.Selector
}

// slotsAfter reports whether a hop after hi has a credential.
func slotsAfter(hops []plan.RouteHop, hi int) bool {
	for j := hi + 1; j < len(hops); j++ {
		if len(hops[j].Slots) > 0 {
			return true
		}
	}
	return false
}

// untried reports whether the hop has a credential besides i not yet excluded.
func untried(excluded []bool, i int) bool {
	for j, x := range excluded {
		if j != i && !x {
			return true
		}
	}
	return false
}

// holdCaller holds the caller's counters for hop, keeping the hold it has when the counters are the same (they differ only for caller rules scoped to models at a host).
func (p *Pipeline) holdCaller(ctx context.Context, st *runState, hop *plan.RouteHop) error {
	cs := hop.CallerCounters()
	if st.caller != nil && sameCounters(st.callerCS, cs) {
		return nil
	}
	st.releaseCaller()
	res, err := p.Limiter.Admit(ctx, cs, limits.Usage{})
	if err != nil {
		var refused *limits.Refused
		if errors.As(err, &refused) {
			return &CallerLimitedError{Refused: refused}
		}
		return err
	}
	st.caller, st.callerCS = res, cs
	return nil
}

func sameCounters(a, b []limits.Counter) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Group != b[i].Group {
			return false
		}
	}
	return true
}

// pick returns the index in hop.Slots of the credential to call next and the breaker record it was picked on; -1 when none is left. Excluded slots are skipped, and so are slots whose account is cooling, noting when it ends on lim.
func (p *Pipeline) pick(ctx context.Context, hop *plan.RouteHop, excluded []bool, lim **UpstreamLimitedError) (int, keypool.Candidate, error) {
	nowMs := time.Now().UnixMilli()
	// The common pick is over the whole hop: its own credential list then serves, and no slice is built.
	keys, at := hop.Credentials, []int(nil)
	if len(keys) != len(hop.Slots) || skipsAny(hop, excluded, nowMs) {
		keys = make([]*hostkey.HostKey, 0, len(hop.Slots))
		at = make([]int, 0, len(hop.Slots))
		for i, s := range hop.Slots {
			if excluded[i] {
				continue
			}
			if s.Account != nil && s.Account.Cooldown.Cooled(nowMs) {
				*lim = noteLimited(*lim, time.Duration(s.Account.Cooldown.Until()-nowMs)*time.Millisecond, nil)
				continue
			}
			keys = append(keys, s.Credential)
			at = append(at, i)
		}
	}
	if len(keys) == 0 {
		return -1, keypool.Candidate{}, nil
	}
	slotOf := func(j int) int {
		if at == nil {
			return j
		}
		return at[j]
	}
	if p.Selector == nil {
		return slotOf(0), keypool.Candidate{Key: keys[0]}, nil
	}
	c, err := p.Selector.PickCandidate(ctx, pickScope(hop), hop.Pool.Selection, keys, nil)
	if err != nil {
		return -1, keypool.Candidate{}, err
	}
	for j, k := range keys {
		if k == c.Key {
			return slotOf(j), c, nil
		}
	}
	return -1, keypool.Candidate{}, keypool.ErrNoHealthyKeys
}

// skipsAny reports whether a pick over hop must leave a slot out: excluded, or its account cooling.
func skipsAny(hop *plan.RouteHop, excluded []bool, nowMs int64) bool {
	for i, s := range hop.Slots {
		if excluded[i] || (s.Account != nil && s.Account.Cooldown.Cooled(nowMs)) {
			return true
		}
	}
	return false
}

// pickScope keys a hop's rotation state by its pool, so every caller drawing on one pool shares one rotation; a credential linked on its own has nothing to rotate.
func pickScope(hop *plan.RouteHop) string {
	if hop.Pool.Pool != nil {
		return hop.Pool.Pool.Meta.ID
	}
	return hop.Slots[0].Credential.Meta.ID
}

// coolAccount skips the slot's account on this pod until the refused account counter frees up: every credential of the account shares that ceiling.
func coolAccount(s *plan.KeySlot, r *limits.Refused) {
	if s.Account == nil || r.Counter.Rung != limits.RungAccount || r.RetryAfter <= 0 {
		return
	}
	s.Account.Cooldown.Extend(time.Now().Add(r.RetryAfter).UnixMilli())
}

// noteLimited keeps the earliest time any refused or cooled credential frees up.
func noteLimited(cur *UpstreamLimitedError, after time.Duration, r *limits.Refused) *UpstreamLimitedError {
	if cur == nil {
		return &UpstreamLimitedError{RetryAfter: after, Refused: r}
	}
	if after < cur.RetryAfter {
		cur.RetryAfter = after
	}
	if r != nil {
		cur.Refused = r
	}
	return cur
}

// stampAttempt records the attempt on the lifecycle before the call: a stream session builds its usage row at stream end, before post-flight runs, and a failure never reaches it.
func stampAttempt(lc *lifecycle.Context, a *attempt, attempts int) {
	if lc == nil {
		return
	}
	lc.Attempts = attempts
	lc.HostKeyID = a.slot.Credential.KeyHash
	lc.HostID, lc.HostName = a.hop.Host.Meta.ID, a.hop.Host.Meta.Name
	lc.PoolID, lc.AccountID = "", ""
	if a.hop.Pool.Pool != nil {
		lc.PoolID = a.hop.Pool.Pool.Meta.ID
	}
	if a.slot.Account != nil {
		lc.AccountID = a.slot.Account.Account.Meta.ID
	}
	lc.PricingID, lc.PricingName = "", ""
	if pr := a.hop.Pricing; pr != nil {
		lc.PricingID, lc.PricingName = pr.Meta.ID, pr.Meta.Name
	}
}

// closeAttempt settles the reservation of an attempt the walk moves on from: kept when the call may have reached the upstream, released otherwise. Detached: the next attempt does not wait on it.
func (st *runState) closeAttempt() {
	if st.cur == nil || st.cur.res == nil {
		st.cur = nil
		return
	}
	// A copy: the next attempt reuses st.att while this one settles.
	a := *st.cur
	st.cur = nil
	go a.close()
}

func (a *attempt) close() {
	if a.reached {
		a.res.Settle(limits.Usage{})
		return
	}
	a.res.Release()
}

func (st *runState) releaseCaller() {
	if st.caller == nil {
		return
	}
	if st.callerReached {
		st.caller.Settle(limits.Usage{})
	} else {
		st.caller.Release()
	}
	st.caller, st.callerCS, st.callerReached = nil, nil, false
}

// abandon settles everything a failed request still holds.
func (st *runState) abandon() {
	if a := st.cur; a != nil && a.res != nil {
		a.close()
		st.cur = nil
	}
	st.releaseCaller()
}

// settle charges the served attempt and the caller with the real usage.
func (st *runState) settle(u limits.Usage) {
	if st.cur.res != nil {
		st.cur.res.Settle(u)
	}
	if st.caller != nil {
		st.caller.Settle(u)
	}
}

// cost prices tokens at the served hop's pricing, at the service tier the upstream reported when it reported one; 0 when the hop is unpriced.
func (st *runState) cost(lc *lifecycle.Context, tokens sdkusage.Tokens) int64 {
	pr := st.cur.hop.Pricing
	if pr == nil || len(tokens) == 0 {
		return 0
	}
	tier := ""
	if s, ok := lc.ResponseSummary(); ok {
		tier = s.ServiceTier
	}
	nanos, _, ok := pr.CostNanosForServiceTier(tokens, tier)
	if !ok {
		return 0
	}
	return nanos
}
