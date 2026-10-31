package pipeline

import (
	"context"

	"github.com/wyolet/relay/app/keypool"
)

// KeyAgent owns what happens when an upstream key fails. The request loop is
// deliberately dumb: it calls OnFailure and obeys the verdict. All the messy
// concerns (secret re-resolution, single-flight, park-and-wait, healing the
// snapshot, alerting) live behind this one method, off the request's import
// graph. A nil KeyAgent on the Pipeline preserves the legacy behavior
// (retryable → fail over, otherwise stop).
//
//   - moreCandidates reports whether another untried key remains. When true,
//     the agent should fail over immediately (VerdictNext) and heal in the
//     background — the request never waits. When false (this key is the last
//     resort), the agent may block on a re-resolve and return VerdictRetry
//     with a fresh value if the key had rotated.
//
// The returned string is the fresh secret value, set only for VerdictRetry.
type KeyAgent interface {
	OnFailure(ctx context.Context, keyID string, kind keypool.FailureKind, moreCandidates bool) (keypool.Verdict, string)
}
