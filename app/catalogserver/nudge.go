package catalogserver

import (
	"context"
	"sync"
	"time"
)

const nudgeSenders = 32

var nudgeRetryDelays = []time.Duration{10 * time.Second, 60 * time.Second, 300 * time.Second}

// nudge tells every live receiver of channel that tag is its new latest. It
// runs in the background: retries wait minutes, and the index never waits on
// them.
func (s *Server) nudge(ctx context.Context, channel, tag string) {
	if s.hooks == nil {
		return
	}
	subs := s.hooks.subs.live(channel, s.now())
	if len(subs) == 0 {
		return
	}
	s.log.Info("catalog-server: nudging receivers", "channel", channel, "tag", tag, "receivers", len(subs))
	s.deliveries.Go(func() { s.deliver(ctx, channel, tag, subs) })
}

// deliver sends one tag's nudge to each receiver and returns once every
// receiver took it, ran out of retries or was superseded by a newer tag.
func (s *Server) deliver(ctx context.Context, channel, tag string, subs []subscription) {
	h := s.hooks
	seq := h.startDelivery(channel)
	payload := map[string]string{"type": "release", "channel": channel, "tag": tag}
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, sub := range subs {
		// The first attempt's slot is taken here so a large channel queues
		// on the slots instead of starting one goroutine per receiver at once.
		if !h.takeSlot(ctx) {
			return
		}
		wg.Go(func() { s.deliverTo(ctx, channel, seq, payload, sub) })
	}
}

// deliverTo runs one receiver's attempts, holding a send slot (taken by the
// caller for the first one) only while a request is in flight. Each retry
// waits from that receiver's own failure, so a slow receiver never delays
// another's retry. A receiver that missed maxFailedTags tags in a row is
// removed.
func (s *Server) deliverTo(ctx context.Context, channel string, seq uint64, payload map[string]string, sub subscription) {
	h := s.hooks
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !h.takeSlot(ctx) {
			return
		}
		delivered := s.sendNudge(ctx, payload, sub)
		<-h.sendSlots
		if delivered {
			return
		}
		if attempt == len(h.retryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.retryDelays[attempt]):
		}
		if h.superseded(channel, seq) {
			// The newer tag's delivery includes every live receiver, and a
			// receiver only needs the newest; this tag counts neither way.
			s.metrics.nudges.WithLabelValues("superseded").Inc()
			return
		}
		if !h.subs.isLive(sub.subKey, s.now()) {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	s.metrics.nudges.WithLabelValues("gave_up").Inc()
	if h.subs.recordTag(sub.subKey, false) {
		s.log.Warn("catalog-server: removed a receiver that missed consecutive releases", "url", sub.url, "channel", sub.channel, "tags", maxFailedTags)
	}
}

// sendNudge posts payload to one receiver and reports whether it took it.
func (s *Server) sendNudge(ctx context.Context, payload map[string]string, sub subscription) bool {
	h := s.hooks
	status, _, err := h.post(ctx, sub.url, sub.addr, payload)
	if err == nil && status >= 200 && status < 300 {
		s.metrics.nudges.WithLabelValues("delivered").Inc()
		h.subs.recordTag(sub.subKey, true)
		return true
	}
	s.metrics.nudges.WithLabelValues("failed").Inc()
	s.log.Debug("catalog-server: nudge failed", "url", sub.url, "status", status, "err", err)
	return false
}

// takeSlot waits for one of the nudgeSenders slots shared by all deliveries.
func (h *webhooks) takeSlot(ctx context.Context) bool {
	select {
	case h.sendSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// startDelivery marks a delivery as its channel's newest and returns its
// number.
func (h *webhooks) startDelivery(channel string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliverySeq[channel]++
	return h.deliverySeq[channel]
}

func (h *webhooks) superseded(channel string, seq uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deliverySeq[channel] != seq
}
