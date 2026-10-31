// Package coord is cross-pod coordination for app components over a shared store. A Notifier publishes rare transitions; a Watcher keeps a local copy of a changed-set current by push, with a version watermark bounding a lost message; a Ticker, an optional helper for components that sync local state on a schedule, sends every participant's commands in one pipeline per beat, keeps the failure clock and counts live processes.
//
// Out of scope: what the commands mean (breakers, cooldowns and their key formats belong to the components), scripts and multi-key atomicity, and the store client itself (callers implement Store). It is not part of package engine and imports only the standard library.
package coord

import "context"

// Op enumerates the store commands the tick and watchers use: plain single-key
// commands, no scripts. Kept narrow because every Op widens what each Store
// implementation must support.
type Op uint8

const (
	OpIncrBy           Op = iota + 1 // Key, Args[0] delta → Int
	OpGet                            // Key → Bytes (nil when missing)
	OpSet                            // Key, Value, Args[1] ttl ms (0 = none)
	OpPExpire                        // Key, Args[1] ttl ms
	OpZAdd                           // Key, Args[0] score, Value member
	OpZCount                         // Key, Args[0] min, Args[1] max → Int
	OpZRangeByScore                  // Key, Args[0] min, Args[1] max → List members, Int highest score among them
	OpZRemRangeByScore               // Key, Args[0] min, Args[1] max
)

// Cmd is one pipelined command. Reply receives the answer; nil Reply discards
// it.
type Cmd struct {
	Op    Op
	Key   string
	Args  [2]int64
	Value []byte
	Reply func(Reply)
}

// Reply is one command's answer.
type Reply struct {
	Int   int64
	Bytes []byte
	List  [][]byte
	Err   error
}

// Store is everything coord needs from the backing store: one ordered
// pipeline round trip and pub/sub.
type Store interface {
	// Pipeline sends cmds in one round trip and calls each Reply in order
	// before returning; a per-command failure goes in Reply.Err. A returned
	// error means the round trip failed and Replies may not have run.
	Pipeline(ctx context.Context, cmds []Cmd) error
	// Publish sends payload to channel's subscribers and returns once ctx ends at the latest.
	Publish(ctx context.Context, channel string, payload []byte) error
	// Subscribe delivers payloads until ctx ends or stop is called,
	// reconnecting internally.
	Subscribe(ctx context.Context, channel string, fn func([]byte)) (stop func(), err error)
}
