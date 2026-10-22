package policy

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/keypool"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Settle is the success post-flight's kv work in one round trip: the inbound and upstream commits (both with obs) and, only when the record the key was picked on needs it, the key's success write. A key picked on a clean closed record, or none, gets no write, so most requests settle with the commits alone. Safe with nil reservations or acquisition.
func (s *Service) Settle(ctx context.Context, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) error {
	var upstream *pkgratelimit.Reservation
	if acq != nil {
		upstream = acq.Reservation
	}
	sel, lim := s.postSelector, s.postLimiter
	if sel == nil {
		if lim == nil {
			return nil
		}
		return lim.CommitBoth(ctx, inbound, upstream, obs)
	}

	b := sel.NewBatch()
	var (
		errs    []error
		commits [2]*pkgratelimit.PreparedCommit
	)
	if lim != nil {
		for i, res := range [2]*pkgratelimit.Reservation{inbound, upstream} {
			pc, err := lim.PrepareCommit(b, res, obs)
			if err != nil {
				errs = append(errs, err)
			}
			commits[i] = pc
		}
	}
	writeSuccess := acq != nil && acq.Key != nil && acq.writeSuccess
	var success *keypool.PreparedSuccess
	if writeSuccess {
		success = sel.PrepareRecordSuccess(b, acq.Key.KeyHash)
	}
	b.Exec(ctx)

	for _, pc := range commits {
		if pc != nil {
			if err := pc.Finish(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	switch {
	case success != nil:
		success.Finish(ctx)
	case writeSuccess:
		sel.RecordSuccess(ctx, acq.Key.KeyHash)
	}
	return errors.Join(errs...)
}
