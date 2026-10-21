package keypool

import (
	"context"

	"github.com/wyolet/relay/pkg/kv"
)

// PreparedSuccess is a RecordSuccess queued on a kv batch; Finish logs the
// transition once the batch has run.
type PreparedSuccess struct {
	s       *Selector
	keyHash string
	result  *kv.BatchResult
}

// PrepareRecordSuccess queues RecordSuccess's script for keyHash on b. It
// returns nil when the store runs no scripts; the caller then calls
// RecordSuccess on its own.
func (s *Selector) PrepareRecordSuccess(b *kv.Batch, keyHash string) *PreparedSuccess {
	if s.runner == nil {
		return nil
	}
	call, err := s.successCall(keyHash)
	if err != nil {
		s.log.Error("keypool: encode record failed", "key_hash", keyHash, "err", err)
		return nil
	}
	return &PreparedSuccess{s: s, keyHash: keyHash, result: b.Script(call)}
}

// Finish logs the success write's outcome.
func (p *PreparedSuccess) Finish(ctx context.Context) {
	p.s.finishSuccess(ctx, p.keyHash, p.result.Value, p.result.Err)
}
