package kv

import (
	"context"
	"errors"
	"time"
)

// ErrNoScripter reports a batched script on a store that runs no scripts.
var ErrNoScripter = errors.New("state: store does not run scripts")

// Batch queues independent commands and runs them together: one round trip on
// Redis (a pipeline; on Cluster one per node holding a key, sent concurrently),
// one call at a time on any other store.
//
// It is not a transaction: each command is atomic on its own, a failed command
// does not undo the others, and no command sees another's result. Each
// script's keys must share a hash tag; keys of different commands need not.
type Batch struct {
	store Store
	ops   []BatchOp
}

// NewBatch returns an empty batch on s.
func NewBatch(s Store) *Batch { return &Batch{store: s} }

// BatchOpKind names the command a BatchOp runs.
type BatchOpKind uint8

const (
	OpGet BatchOpKind = iota
	OpIncr
	OpExpireNX
	OpScript
)

// BatchOp is one queued command, exported so a store can run a batch its own
// way (see Batcher).
type BatchOp struct {
	Kind   BatchOpKind
	Key    string        // Get, Incr, ExpireNX
	Delta  int64         // Incr
	TTL    time.Duration // ExpireNX
	Script ScriptCall    // Script
	Result *BatchResult
}

// BatchResult is one command's outcome, filled by Exec. Get reports a missing
// key as ErrNotFound; Incr fills Int; ExpireNX sets Int to 1 when it set the
// TTL; Script fills Value as RunScript would. A transport failure lands in
// every result's Err.
type BatchResult struct {
	Value []byte
	Int   int64
	Err   error
}

// Batcher is implemented by a store that runs a batch in fewer round trips
// than one per command. ExecBatch fills every op's Result and keeps no
// reference to ops.
type Batcher interface {
	ExecBatch(ctx context.Context, ops []BatchOp)
}

// Get queues a GET of key.
func (b *Batch) Get(key string) *BatchResult {
	return b.add(BatchOp{Kind: OpGet, Key: key})
}

// Incr queues an INCRBY of key by delta.
func (b *Batch) Incr(key string, delta int64) *BatchResult {
	return b.add(BatchOp{Kind: OpIncr, Key: key, Delta: delta})
}

// ExpireNX queues an EXPIRE NX: key gets ttl only when it has none, so a
// key's existing deadline is never pushed out.
func (b *Batch) ExpireNX(key string, ttl time.Duration) *BatchResult {
	return b.add(BatchOp{Kind: OpExpireNX, Key: key, TTL: ttl})
}

// Script queues a script call.
func (b *Batch) Script(call ScriptCall) *BatchResult {
	return b.add(BatchOp{Kind: OpScript, Script: call})
}

func (b *Batch) add(op BatchOp) *BatchResult {
	op.Result = &BatchResult{}
	b.ops = append(b.ops, op)
	return op.Result
}

// Exec runs the queued commands and empties the batch. Each result carries
// its own outcome; there is no batch-level error.
func (b *Batch) Exec(ctx context.Context) {
	ops := b.ops
	b.ops = nil
	if len(ops) == 0 {
		return
	}
	if p, ok := b.store.(Batcher); ok {
		p.ExecBatch(ctx, ops)
		return
	}
	ExecInOrder(ctx, b.store, ops)
}

// ExecInOrder runs ops one at a time through s, which is how a batch runs on
// a store that is not a Batcher. kv.Mem is deliberately not one, so a test
// double wrapping it still sees every command.
func ExecInOrder(ctx context.Context, s Store, ops []BatchOp) {
	for _, op := range ops {
		r := op.Result
		switch op.Kind {
		case OpGet:
			r.Value, r.Err = s.Get(ctx, op.Key)
		case OpIncr:
			r.Int, r.Err = s.Incr(ctx, op.Key, op.Delta)
		case OpExpireNX:
			nx, ok := s.(interface {
				ExpireNX(ctx context.Context, key string, ttl time.Duration) (bool, error)
			})
			if !ok {
				r.Err = errors.ErrUnsupported
				continue
			}
			set, err := nx.ExpireNX(ctx, op.Key, op.TTL)
			if set {
				r.Int = 1
			}
			r.Err = err
		case OpScript:
			sr, ok := s.(Scripter)
			if !ok {
				r.Err = ErrNoScripter
				continue
			}
			c := op.Script
			r.Value, r.Err = sr.RunScript(ctx, c.Name, c.Script, c.Keys, c.Args...)
		}
	}
}
