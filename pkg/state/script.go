package state

import "context"

// ScriptRunner is an optional interface implemented by stores that support
// named Lua (or equivalent) scripts. Consumers type-assert to opt in.
type ScriptRunner interface {
	RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error)
}

// ScriptImpl is the Go-emulator function registered on MemStore.
// It receives the store so it can call Get/Set/Incr etc. directly.
type ScriptImpl func(ctx context.Context, store *MemStore, keys []string, args []any) ([]byte, error)
