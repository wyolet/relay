package kv

import "context"

// ScriptRunner is an optional interface implemented by stores that support
// named Lua (or equivalent) scripts. Consumers type-assert to opt in.
type Scripter interface {
	RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error)
}

// ScriptImpl is the Go-emulator function registered on MemStore.
// It receives the store so it can call Get/Set/Incr etc. directly.
type ScriptImpl func(ctx context.Context, store *Mem, keys []string, args []any) ([]byte, error)

// ScriptCall describes one script invocation for a batch.
type ScriptCall struct {
	Name   string
	Script string
	Keys   []string
	Args   []any
}

// ScriptResult is the positional result of one ScriptCall in a batch.
type ScriptResult struct {
	Value []byte
	Err   error
}

// BatchScripter is an optional interface implemented by stores that can run
// several independent scripts in a single network round trip (a Redis
// pipeline). Consumers type-assert to opt in.
//
// Atomicity: each call is individually atomic on its own Cluster slot; the
// batch provides NO cross-call atomicity. Keys of DIFFERENT calls may live on
// different slots (each call's own keys must still share a hash tag). Use it
// only for independent operations — e.g. committing two rate-limit
// reservations that live under different hash tags and therefore cannot share
// one CROSSSLOT-safe script.
//
// Results are positional (one per input call, same order); a per-call failure
// is carried in ScriptResult.Err rather than aborting the batch.
type BatchScripter interface {
	RunScriptBatch(ctx context.Context, calls []ScriptCall) []ScriptResult
}
