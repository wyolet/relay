package control

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PlanWaiter waits until this pod's published plans serve a write; *plan.Holder satisfies it. A client often spends a fresh token or key the moment it gets one, and the data plane reads only the plans, which pick a write up asynchronously.
type PlanWaiter interface {
	AwaitOpenLane(ctx context.Context, accountID string) bool
	AwaitKey(ctx context.Context, keyHash string) bool
}

// planWait bounds how long a write waits for the plans before answering anyway.
const planWait = 5 * time.Second

// awaitServed waits, bounded by planWait and the request, for await to report the write served. A timeout leaves the write standing and the answer going out; it is logged, since the caller's first request may be refused until the plans catch up.
func awaitServed(ctx context.Context, what, id string, await func(context.Context) bool) {
	ctx, cancel := context.WithTimeout(ctx, planWait)
	defer cancel()
	if !await(ctx) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.WarnContext(ctx, "control: answering before this pod's plans serve the write", "what", what, "id", id, "waited", planWait)
	}
}
