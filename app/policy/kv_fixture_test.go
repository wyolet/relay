package policy

import (
	"testing"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// recordingMem is an in-memory store with the limiter and key-pool scripts
// registered, recording every script it runs.
func recordingMem(t testing.TB) *kvtest.Recording {
	t.Helper()
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	return kvtest.NewRecording(mem)
}

// scriptNames lists the scripts rec ran, in order.
func scriptNames(rec *kvtest.Recording) []string {
	var names []string
	for _, c := range rec.Scripts() {
		names = append(names, c.Name)
	}
	return names
}

// reserveCalls counts the reservation scripts rec ran.
func reserveCalls(rec *kvtest.Recording) int { return len(rec.Named("limit.reserve")) }

// lastKeys is the key list of the last script rec ran.
func lastKeys(rec *kvtest.Recording) []string {
	scripts := rec.Scripts()
	return scripts[len(scripts)-1].Keys
}

// batchStats reports how many batched round trips rec ran and how many
// scripts the last one carried.
func batchStats(rec *kvtest.Recording) (count, lastSize int) {
	sizes := rec.Batches()
	if len(sizes) == 0 {
		return 0, 0
	}
	return len(sizes), sizes[len(sizes)-1]
}
