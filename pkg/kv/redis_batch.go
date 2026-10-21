package kv

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ExpireNX sets ttl on key only when it has no expiry (EXPIRE NX, Redis 7.0+ and every Valkey) and reports whether it did.
func (r *Redis) ExpireNX(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	start := time.Now()
	ok, err := r.client.ExpireNX(ctx, key, ttl).Result()
	r.trackSlow("expire_nx", key, start, err)
	return ok, markUnavailable(err)
}

// ExecBatch implements Batcher with one pipeline: one round trip on a single node, one per involved node on Cluster (go-redis sends those concurrently).
func (r *Redis) ExecBatch(ctx context.Context, ops []BatchOp) {
	if len(ops) == 0 {
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Done()

	// A script whose SHA can't be preloaded goes out as plain EVAL.
	shas := make([]string, len(ops))
	for i, op := range ops {
		if op.Kind == OpScript {
			if sha, err := r.loadSHA(ctx, op.Script.Name, op.Script.Script); err == nil {
				shas[i] = sha
			}
		}
	}

	cmds := make([]redis.Cmder, len(ops))
	start := time.Now()
	_, err := r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, op := range ops {
			switch op.Kind {
			case OpGet:
				cmds[i] = p.Get(ctx, op.Key)
			case OpIncr:
				cmds[i] = p.IncrBy(ctx, op.Key, op.Delta)
			case OpExpireNX:
				cmds[i] = p.ExpireNX(ctx, op.Key, op.TTL)
			case OpScript:
				c := op.Script
				if shas[i] != "" {
					cmds[i] = p.EvalSha(ctx, shas[i], c.Keys, c.Args...)
				} else {
					cmds[i] = p.Eval(ctx, c.Script, c.Keys, c.Args...)
				}
			}
		}
		return nil
	})
	// Pipelined returns the first command's error; a command's own error reply says nothing about the batch.
	var reply redis.Error
	if errors.As(err, &reply) && !serverCannotServe(err) {
		err = nil
	}
	r.trackSlow("batch", "", start, err)

	for i, op := range ops {
		res := op.Result
		switch c := cmds[i].(type) {
		case *redis.StringCmd:
			v, err := c.Bytes()
			if errors.Is(err, redis.Nil) {
				res.Err = ErrNotFound
				continue
			}
			res.Value, res.Err = v, markUnavailable(err)
		case *redis.IntCmd:
			n, err := c.Result()
			res.Int, res.Err = n, markUnavailable(err)
		case *redis.BoolCmd:
			set, err := c.Result()
			if set {
				res.Int = 1
			}
			res.Err = markUnavailable(err)
		case *redis.Cmd:
			v, err := c.Result()
			if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
				// Evicted between preload and exec: rerun this script alone, with runLua's reload and EVAL fallback.
				r.shas.Delete(op.Script.Name)
				s := op.Script
				b, err := r.runLua(ctx, s.Name, s.Script, s.Keys, s.Args...)
				res.Value, res.Err = b, markUnavailable(err)
				continue
			}
			if err != nil {
				res.Err = markUnavailable(err)
				continue
			}
			res.Value, res.Err = redisResultToBytes(v)
		}
	}
}
