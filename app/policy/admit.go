package policy

import (
	"context"

	"github.com/wyolet/relay/app/keypool"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Admission is a request's first attempt as Admit decided it: the inbound reservation, and what Acquire would have returned for the first key.
type Admission struct {
	Inbound *pkgratelimit.Reservation
	// Acq and AcqErr are the first key's acquisition. The inbound reservation stands whether or not it succeeded; the caller settles it.
	Acq    *Acquisition
	AcqErr error
}

// Admit is ReserveInbound followed by Acquire for a request's first attempt, with the inbound reserve script and the key pick's reads sent in one kv batch (one round trip). The selector and limiter must share a store, as every composition does. A refused or failed reserve returns its error alone, exactly as ReserveInbound would. A failover re-picks with Acquire.
func (s *Service) Admit(ctx context.Context, in InboundInput, acq AcquireInput) (Admission, error) {
	if s.selector == nil || s.limiter == nil {
		inbound, err := s.ReserveInbound(ctx, in)
		if err != nil {
			return Admission{}, err
		}
		a, err := s.Acquire(ctx, acq)
		return Admission{Inbound: inbound, Acq: a, AcqErr: err}, nil
	}

	scope, algo := pickScope(acq.Policy)
	b := s.selector.NewBatch(1 + keypool.PickOps(algo, len(acq.Keys)))
	var reserve *pkgratelimit.PreparedReserve
	rules, metered := s.inboundRules(ctx, in)
	if len(rules) > 0 {
		var err error
		if reserve, err = s.limiter.PrepareReserve(b, inboundScope(in), rules); err != nil {
			return Admission{}, err
		}
		b.ShedWhenPoolBusy()
	}
	excluded, full := s.full.exclude(acq.Keys, acq.Excluded, modelIDOf(acq), s.clock())
	var pick *keypool.PreparedPick
	if full == nil || !allExcluded(acq.Keys, excluded) {
		pick = s.selector.PreparePick(b, scope, algo, acq.Keys, excluded)
	}
	b.Exec(ctx)

	var adm Admission
	if reserve != nil {
		res, err := reserve.Finish(ctx)
		if err != nil {
			// The pick is not evaluated; its INCR only moved the rotation start.
			return Admission{}, err
		}
		if !metered {
			res.SetNoCommit()
		}
		adm.Inbound = res
	}
	if pick == nil {
		adm.AcqErr = keyTiersFullError(full)
	} else if picked, err := pick.Finish(ctx); err != nil {
		adm.AcqErr = pickError(err, full)
	} else {
		adm.Acq, adm.AcqErr = s.reserveTier(ctx, picked, acq)
	}
	return adm, nil
}
