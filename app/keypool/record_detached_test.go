package keypool

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// cancelAwareMem refuses scripts on a cancelled context, as a networked store does.
type cancelAwareMem struct{ *kv.Mem }

func (m cancelAwareMem) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.Mem.RunScript(ctx, name, script, keys, args...)
}

// A failure seen by a request whose caller has since gone is still written.
func TestRecordFailure_WritesAfterCallerLeft(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	RegisterScripts(mem)
	sel := New(cancelAwareMem{mem}, noopLogger(), func() time.Time { return time.Unix(1_800_000_000, 0) }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sel.RecordFailure(ctx, "hash-a", FailureAuth, 0)

	rec, found := sel.ReadCircuit(context.Background(), "hash-a")
	if !found || rec.State != CircuitOpen {
		t.Fatalf("record = %+v (found %v), want an open key", rec, found)
	}
}
