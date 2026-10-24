package kv

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// poolSize is the client's pool size after go-redis applied its default. On Cluster it is per node, so the wait bound, counted across the client, is tighter there.
func poolSize(client redis.UniversalClient, configured int) int {
	switch c := client.(type) {
	case *redis.Client:
		return c.Options().PoolSize
	case *redis.ClusterClient:
		return c.Options().PoolSize
	}
	return configured
}

// enter counts a call that holds or waits for a pool connection; leave ends it. Past the pool size the count is the callers queued in go-redis, which exposes no such number of its own.
func (r *Redis) enter() { r.calls.Add(1) }
func (r *Redis) leave() { r.calls.Add(-1) }

// enterOrShed counts a call unless the calls already holding or waiting for a connection are at the bound.
func (r *Redis) enterOrShed() bool {
	if r.calls.Add(1) > r.callLimit {
		r.calls.Add(-1)
		return false
	}
	return true
}

// PoolBusy implements PoolShedder.
func (r *Redis) PoolBusy() bool { return r.calls.Load() >= r.callLimit }

// ExecBatchOrShed implements PoolShedder.
func (r *Redis) ExecBatchOrShed(ctx context.Context, ops []BatchOp) {
	if len(ops) == 0 {
		return
	}
	if !r.enterOrShed() {
		for _, op := range ops {
			op.Result.Err = ErrPoolBusy
		}
		return
	}
	defer r.leave()
	r.execBatch(ctx, ops)
}

// RunScriptOrShed implements PoolShedder.
func (r *Redis) RunScriptOrShed(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if !r.enterOrShed() {
		return nil, ErrPoolBusy
	}
	defer r.leave()
	return r.runScript(ctx, name, script, keys, args...)
}
