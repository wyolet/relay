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

// deliver sends one tag's nudge until each receiver took it or its retries
// ran out; a receiver that missed maxFailedTags tags in a row is removed.
func (s *Server) deliver(ctx context.Context, channel, tag string, pending []subscription) {
	h := s.hooks
	payload := map[string]string{"type": "release", "channel": channel, "tag": tag}
	for attempt := 0; ; attempt++ {
		pending = s.sendRound(ctx, payload, pending)
		if len(pending) == 0 || attempt == len(h.retryDelays) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.retryDelays[attempt]):
		}
		now := s.now()
		live := pending[:0]
		for _, sub := range pending {
			if h.subs.isLive(sub.subKey, now) {
				live = append(live, sub)
			}
		}
		pending = live
	}
	if ctx.Err() != nil {
		return
	}
	for _, sub := range pending {
		s.metrics.nudges.WithLabelValues("gave_up").Inc()
		if h.subs.recordTag(sub.subKey, false) {
			s.log.Warn("catalog-server: removed a receiver that missed consecutive releases", "url", sub.url, "channel", sub.channel, "tags", maxFailedTags)
		}
	}
}

// sendRound posts payload to each receiver, at most nudgeSenders at once
// across all deliveries, and returns the ones that failed.
func (s *Server) sendRound(ctx context.Context, payload map[string]string, subs []subscription) []subscription {
	h := s.hooks
	var (
		mu     sync.Mutex
		failed []subscription
		wg     sync.WaitGroup
	)
	defer wg.Wait()
	for _, sub := range subs {
		select {
		case h.sendSlots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		wg.Go(func() {
			defer func() { <-h.sendSlots }()
			status, _, err := h.post(ctx, sub.url, sub.addr, payload)
			if err == nil && status >= 200 && status < 300 {
				s.metrics.nudges.WithLabelValues("delivered").Inc()
				h.subs.recordTag(sub.subKey, true)
				return
			}
			s.metrics.nudges.WithLabelValues("failed").Inc()
			s.log.Debug("catalog-server: nudge failed", "url", sub.url, "status", status, "err", err)
			mu.Lock()
			failed = append(failed, sub)
			mu.Unlock()
		})
	}
	wg.Wait()
	return failed
}
