package catalogserver

import (
	"net/http"
	"testing"
	"time"
)

func TestSubscriptions_ExpireAfterTTL(t *testing.T) {
	s, res := newHookServer(t, true)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	rc := newReceiver(t, false)
	res.set("relay.test", addrs("127.0.0.1"))
	if rec := register(t, s, "http://relay.test:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("register: %d", rec.Code)
	}
	if n := subscriptionsGauge(t, s, testChannel); n != 1 {
		t.Fatalf("subscriptions gauge %v, want 1", n)
	}
	now = now.Add(24*time.Hour - time.Second)
	if len(s.hooks.subs.live(testChannel, now)) != 1 {
		t.Fatal("expired before its TTL")
	}
	now = now.Add(time.Second)
	if len(s.hooks.subs.live(testChannel, now)) != 0 {
		t.Fatal("live after its TTL")
	}
	s.nudge(t.Context(), testChannel, "v0.1.1")
	s.deliveries.Wait()
	if rc.releaseCount() != 0 {
		t.Fatal("an expired subscription was nudged")
	}
	if n := subscriptionsGauge(t, s, testChannel); n != 0 {
		t.Fatalf("subscriptions gauge %v after expiry, want 0", n)
	}
	s.hooks.subs.sweep(now)
	if subCount(s) != 0 {
		t.Fatal("sweep kept an expired subscription")
	}
}

func subscriptionsGauge(t *testing.T, s *Server, channel string) float64 {
	t.Helper()
	families := must(s.metrics.registry.Gather())(t)
	for _, f := range families {
		if f.GetName() != "catalog_server_webhook_subscriptions" {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetLabel()[0].GetValue() == channel {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}
