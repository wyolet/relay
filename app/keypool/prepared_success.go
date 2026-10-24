package keypool

import (
	"context"

	"github.com/wyolet/relay/pkg/kv"
)

// PreparedSuccess is a RecordSuccess queued on a kv batch; Finish logs the transition once the batch has run.
type PreparedSuccess struct {
	s       *Selector
	keyHash string
	result  *kv.BatchResult
}

// PrepareRecordSuccess queues RecordSuccess's script for keyHash on b. It returns nil when the store runs no scripts; the caller then calls RecordSuccess on its own.
func (s *Selector) PrepareRecordSuccess(b *kv.Batch, keyHash string) *PreparedSuccess {
	if s.runner == nil {
		return nil
	}
	return &PreparedSuccess{s: s, keyHash: keyHash, result: b.Script(successCall(keyHash, s.clock()))}
}

// Err is the success write's outcome as the batch left it, before Finish logs it.
func (p *PreparedSuccess) Err() error { return p.result.Err }

// Finish logs the success write's outcome.
func (p *PreparedSuccess) Finish(ctx context.Context) {
	p.s.finishSuccess(ctx, p.keyHash, p.result.Value, p.result.Err)
}
