package keypool

import (
	"context"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// PreparedFailure is a RecordFailure queued on a kv batch; Finish logs the transition once the batch has run.
type PreparedFailure struct {
	s         *Selector
	keyHash   string
	op        failureOp
	logReason string
	result    *kv.BatchResult
}

// PrepareRecordFailure queues RecordFailure's script for keyHash on b. It returns nil when there is no script to queue (the store runs none, or kind writes nothing); the caller then calls RecordFailure on its own.
func (s *Selector) PrepareRecordFailure(b *kv.Batch, keyHash string, kind FailureKind, retryAfter time.Duration) *PreparedFailure {
	if s.runner == nil || isAnonymous(keyHash) {
		return nil
	}
	op, logReason, ok := s.failureOpFor(kind, retryAfter)
	if !ok {
		return nil
	}
	return &PreparedFailure{s: s, keyHash: keyHash, op: op, logReason: logReason, result: b.Script(failureCall(keyHash, op))}
}

// Finish logs the failure write's outcome.
func (p *PreparedFailure) Finish(ctx context.Context) {
	p.s.finishFailure(ctx, p.keyHash, p.op, p.logReason, p.result.Value, p.result.Err)
}
