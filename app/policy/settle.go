package policy

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Settle is the success post-flight's kv work in one round trip: the inbound and upstream commits (both with obs) and, only when the record the key was picked on needs it, the key's success write. A key picked on a clean closed record, or none, gets no write, so most requests settle with the commits alone. With a settle queue (WithSettleQueue) that round trip is shared with other requests' settles; a full queue settles on a round trip of its own. Safe with nil reservations or acquisition.
func (s *Service) Settle(ctx context.Context, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) error {
	if q := s.settleQueue; q != nil && s.settleWrites(inbound, acq, obs) {
		if queued, err := q.settle(ctx, inbound, acq, obs); queued {
			return err
		}
	}
	return s.settleNow(ctx, inbound, acq, obs)
}

// settleWrites reports whether settling makes any kv call, so a settle with nothing to write never waits on the queue.
func (s *Service) settleWrites(inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) bool {
	if acq != nil && acq.Key != nil && acq.writeSuccess {
		return true
	}
	if s.postLimiter == nil {
		return false
	}
	return inbound.NeedsCommit(obs) || (acq != nil && acq.Reservation.NeedsCommit(obs))
}

// settleNow settles on a round trip of its own.
func (s *Service) settleNow(ctx context.Context, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) error {
	sel, lim := s.postSelector, s.postLimiter
	if sel == nil {
		if lim == nil {
			return nil
		}
		var upstream *pkgratelimit.Reservation
		if acq != nil {
			upstream = acq.Reservation
		}
		return lim.CommitBoth(ctx, inbound, upstream, obs)
	}
	b := sel.NewBatch()
	p := s.prepareSettle(b, inbound, acq, obs)
	b.Exec(ctx)
	return p.finish(ctx, ctx)
}

// preparedSettle is one request's settle queued on a kv batch; finish reads its outcome once the batch has run.
type preparedSettle struct {
	sel     *keypool.Selector
	errs    []error
	commits [2]*pkgratelimit.PreparedCommit
	success *keypool.PreparedSuccess
	// successKey is a success write the store could not batch, made on its own in finish.
	successKey string
}

// prepareSettle queues the commits and the success write of one settle on b. Requires a post-flight selector.
func (s *Service) prepareSettle(b *kv.Batch, inbound *pkgratelimit.Reservation, acq *Acquisition, obs pkgratelimit.Observations) preparedSettle {
	p := preparedSettle{sel: s.postSelector}
	var upstream *pkgratelimit.Reservation
	if acq != nil {
		upstream = acq.Reservation
	}
	if lim := s.postLimiter; lim != nil {
		for i, res := range [2]*pkgratelimit.Reservation{inbound, upstream} {
			pc, err := lim.PrepareCommit(b, res, obs)
			if err != nil {
				p.errs = append(p.errs, err)
			}
			p.commits[i] = pc
		}
	}
	if acq != nil && acq.Key != nil && acq.writeSuccess {
		if p.success = p.sel.PrepareRecordSuccess(b, acq.Key.KeyHash); p.success == nil {
			p.successKey = acq.Key.KeyHash
		}
	}
	return p
}

// transportFailed reports whether any of the settle's batched commands failed for the transport rather than with its own error reply: whether it ran is unknown.
func (p *preparedSettle) transportFailed() bool {
	for _, pc := range p.commits {
		if pc != nil && isTransportError(pc.Err()) {
			return true
		}
	}
	return p.success != nil && isTransportError(p.success.Err())
}

func isTransportError(err error) bool {
	return errors.Is(err, kv.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// finish returns the settle's failures once its batch has run. logCtx carries the request's log fields; ctx bounds the unbatched success write.
func (p *preparedSettle) finish(logCtx, ctx context.Context) error {
	for _, pc := range p.commits {
		if pc != nil {
			if err := pc.Finish(); err != nil {
				p.errs = append(p.errs, err)
			}
		}
	}
	switch {
	case p.success != nil:
		p.success.Finish(logCtx)
	case p.successKey != "":
		p.sel.RecordSuccess(ctx, p.successKey)
	}
	return errors.Join(p.errs...)
}
