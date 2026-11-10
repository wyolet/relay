package catalogserver

import (
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	subscriptionTTL   = 24 * time.Hour
	maxSubscriptions  = 10_000
	maxFailedTags     = 3
	registrationLimit = 10
	registrationSpan  = time.Minute
	// Bounds the limiter's memory when many addresses register within a span.
	maxLimitedClients = 100_000
)

type subKey struct {
	url     string
	channel string
}

type subscription struct {
	subKey
	addr       netip.Addr // the address the URL's host resolved to at registration
	expiresAt  time.Time
	failedTags int
}

type rateWindow struct {
	start time.Time
	n     int
}

// subscriptions holds this replica's receivers in memory. A restart loses
// them until relays renew; their polling covers the gap.
type subscriptions struct {
	mu      sync.Mutex
	subs    map[subKey]*subscription
	clients map[netip.Prefix]*rateWindow
}

func newSubscriptions() *subscriptions {
	return &subscriptions{subs: map[subKey]*subscription{}, clients: map[netip.Prefix]*rateWindow{}}
}

// allowRegistration counts one attempt from client, keyed by its /64 for
// IPv6 since one host usually holds the whole prefix.
func (s *subscriptions) allowRegistration(client netip.Addr, now time.Time) bool {
	client = client.Unmap()
	bits := 32
	if client.Is6() {
		bits = 64
	}
	key, err := client.Prefix(bits)
	if err != nil {
		key = netip.Prefix{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.clients[key]
	if w == nil {
		if len(s.clients) >= maxLimitedClients {
			return false
		}
		w = &rateWindow{start: now}
		s.clients[key] = w
	}
	if now.Sub(w.start) >= registrationSpan {
		w.start, w.n = now, 0
	}
	w.n++
	return w.n <= registrationLimit
}

// hasRoom is the cheap check before a challenge; put checks again.
func (s *subscriptions) hasRoom(k subKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.subs[k]
	return ok || len(s.subs) < maxSubscriptions
}

// put adds or renews k. A renewal passed a fresh challenge, so it also
// clears the failure count. ok is false when the store is full.
func (s *subscriptions) put(k subKey, addr netip.Addr, now time.Time) (expires time.Time, renewed, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[k]
	renewed = sub != nil && now.Before(sub.expiresAt)
	if sub == nil {
		if len(s.subs) >= maxSubscriptions {
			return time.Time{}, false, false
		}
		sub = &subscription{subKey: k}
		s.subs[k] = sub
	}
	sub.addr, sub.expiresAt, sub.failedTags = addr, now.Add(subscriptionTTL), 0
	return sub.expiresAt, renewed, true
}

// live copies the unexpired subscriptions of channel.
func (s *subscriptions) live(channel string, now time.Time) []subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []subscription
	for _, sub := range s.subs {
		if sub.channel == channel && now.Before(sub.expiresAt) {
			out = append(out, *sub)
		}
	}
	return out
}

func (s *subscriptions) isLive(k subKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[k]
	return sub != nil && now.Before(sub.expiresAt)
}

// recordTag notes whether k got one tag's nudge, and removes k once it has
// missed maxFailedTags tags in a row. It reports whether k was removed.
func (s *subscriptions) recordTag(k subKey, delivered bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[k]
	if sub == nil {
		return false
	}
	if delivered {
		sub.failedTags = 0
		return false
	}
	sub.failedTags++
	if sub.failedTags < maxFailedTags {
		return false
	}
	delete(s.subs, k)
	return true
}

// sweep drops expired subscriptions and finished rate-limit windows.
func (s *subscriptions) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sub := range s.subs {
		if !now.Before(sub.expiresAt) {
			delete(s.subs, k)
		}
	}
	for k, w := range s.clients {
		if now.Sub(w.start) >= registrationSpan {
			delete(s.clients, k)
		}
	}
}

func (s *subscriptions) countByChannel(now time.Time) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, sub := range s.subs {
		if now.Before(sub.expiresAt) {
			out[sub.channel]++
		}
	}
	return out
}

// subscriptionsCollector reports live subscriptions per channel at scrape time.
type subscriptionsCollector struct {
	desc *prometheus.Desc
	subs *subscriptions
	now  func() time.Time
}

func (c subscriptionsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c subscriptionsCollector) Collect(ch chan<- prometheus.Metric) {
	for channel, n := range c.subs.countByChannel(c.now()) {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), channel)
	}
}
