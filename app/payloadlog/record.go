package payloadlog

import "github.com/wyolet/relay/pkg/payload"

// Namespace is the key under which the payload Record is attached to the
// lifecycle Context. SinkCollector reads it back via lc.Collected.
// Distinct from usagelog's "usage" so the two observers coexist on the
// same Registry.
const Namespace = "payload"

// Record / Sink / Closer are re-exported from pkg/payload so the observer
// and its wiring can import just payloadlog. The contract + backends live
// in pkg/payload and pkg/payload/<backend> (file, s3) — vendorable, and
// the s3 driver is excludable from minimal builds.
type (
	Record = payload.Record
	Sink   = payload.Sink
	Closer = payload.Closer

	// Reader / Query are the read-side contract, re-exported so the control
	// plane imports just payloadlog.
	Reader = payload.Reader
	Query  = payload.Query
)

// ErrNotFound is re-exported so control handlers can map an absent capture
// to 404 without importing pkg/payload directly.
var ErrNotFound = payload.ErrNotFound

// clip truncates b to max bytes, reporting whether it was cut. max <= 0
// means no cap. The returned slice aliases b (no copy) — callers must not
// mutate it, which holds: bodies are read-only after capture.
func clip(b []byte, max int) ([]byte, bool) {
	if max > 0 && len(b) > max {
		return b[:max], true
	}
	return b, false
}
