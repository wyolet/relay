package pipeline

import (
	"context"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
)

// settleQueued settles a served request through the settle queue: the caller's and the attempt's commits and, when the picked record needs it, the key's success write, in one shared round trip. false when there is no queue or a reservation cannot queue its settle; the caller then settles directly.
func (p *Pipeline) settleQueued(ctx context.Context, st *runState, u limits.Usage) bool {
	if p.Settles == nil {
		return false
	}
	caller, ok := batchSettler(st.caller)
	if !ok {
		return false
	}
	upstream, ok := batchSettler(st.cur.res)
	if !ok {
		return false
	}
	b := p.Settles.NewBatch()
	w := &servedWrites{}
	for _, bs := range [2]limits.BatchSettler{caller, upstream} {
		if bs != nil {
			w.parts = append(w.parts, bs.PrepareSettle(b, u))
		}
	}
	if sel := p.postSelector(); sel != nil && st.cur.cand.SuccessNeedsWrite() {
		w.sel, w.key = sel, st.cur.slot.Credential.KeyHash
		w.success = sel.PrepareRecordSuccess(b, w.key)
	}
	p.Settles.Settle(ctx, b, w)
	return true
}

// batchSettler returns r as a BatchSettler; nil with true for no reservation, false for one that cannot queue its settle.
func batchSettler(r limits.Reservation) (limits.BatchSettler, bool) {
	if r == nil {
		return nil, true
	}
	bs, ok := r.(limits.BatchSettler)
	return bs, ok
}

// servedWrites is a served request's settle queued on a batch.
type servedWrites struct {
	parts   []limits.PreparedSettle
	sel     *keypool.Selector
	key     string
	success *keypool.PreparedSuccess
}

// TransportFailed looks at the commits only: the success write is never resent, so its failure asks for nothing.
func (w *servedWrites) TransportFailed() bool {
	for _, p := range w.parts {
		if p.TransportFailed() {
			return true
		}
	}
	return false
}

func (w *servedWrites) Finish(ctx context.Context) {
	for _, p := range w.parts {
		p.Finish(ctx)
	}
	w.finishSuccess(ctx)
}

// Resend writes the commits again. The success write is not sent again: it takes a backoff step off and clears failure counts other processes added, so a second run would apply twice.
func (w *servedWrites) Resend(ctx context.Context) {
	for _, p := range w.parts {
		p.Resend(ctx)
	}
	w.finishSuccess(ctx)
}

// finishSuccess reads the batched success write's outcome, or makes one the store could not batch on its own.
func (w *servedWrites) finishSuccess(ctx context.Context) {
	switch {
	case w.success != nil:
		w.success.Finish(ctx)
	case w.key != "":
		w.sel.RecordSuccess(ctx, w.key)
	}
}
