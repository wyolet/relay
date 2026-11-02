package plan

import (
	"context"
	"time"
)

// nextPublish is closed by the next publish.
func (h *Holder) nextPublish() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.published == nil {
		h.published = make(chan struct{})
	}
	return h.published
}

// Await blocks until the published plans satisfy cond or ctx ends, and reports whether they did. It wakes on each publish rather than polling, so a write followed by Await returns as soon as this pod serves the result.
func (h *Holder) Await(ctx context.Context, cond func(*Root) bool) bool {
	for {
		// Taken before the check, so a publish between the check and the wait still wakes it.
		next := h.nextPublish()
		if r := h.Load(); r != nil && cond(r) {
			return true
		}
		select {
		case <-next:
		case <-ctx.Done():
			return false
		}
	}
}

// AwaitKey waits until the published plans hold the key plan for keyHash, refusing or not: the point the key authenticates on this pod.
func (h *Holder) AwaitKey(ctx context.Context, keyHash string) bool {
	return h.Await(ctx, func(r *Root) bool { return r.Keys.Lookup(keyHash, time.Now()) != nil })
}

// AwaitOpenLane waits until the published plans hold an open lane for accountID: the point a request in that account can be served on this pod.
func (h *Holder) AwaitOpenLane(ctx context.Context, accountID string) bool {
	return h.Await(ctx, func(r *Root) bool {
		cp := r.Lanes.Lookup(accountID)
		return cp != nil && !cp.Lane.Spec.Closed
	})
}
