package catalogserver

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRefusedAddr(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1": true, "127.8.9.1": true, "::1": true,
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.254": true, "192.168.1.1": true,
		"169.254.169.254": true, "fe80::1": true,
		"100.64.0.1": true, "100.127.255.254": true,
		"fc00::1": true, "fd12:3456::1": true,
		"0.0.0.0": true, "::": true, "0.1.2.3": true,
		"224.0.0.1": true, "239.1.1.1": true, "ff02::1": true, "ff05::2": true,
		"255.255.255.255": true, "240.0.0.1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "::ffff:169.254.169.254": true,
		"64:ff9b::a00:1": true,
		"8.8.8.8":        false, "1.1.1.1": false, "100.63.255.255": false, "100.128.0.1": false,
		"172.32.0.1": false, "2606:4700::1111": false, "::ffff:8.8.8.8": false,
	} {
		if got := refusedAddr(netip.MustParseAddr(addr)); got != refused {
			t.Errorf("%s: refused %v, want %v", addr, got, refused)
		}
	}
}

func TestRegister_RefusesNonPublicAddresses(t *testing.T) {
	s, res := newHookServer(t, false)
	rc := newReceiver(t, true)
	res.set("internal.example", addrs("10.0.0.5"))
	res.set("metadata.example", addrs("169.254.169.254"))
	res.set("mixed.example", addrs("203.0.113.10", "10.0.0.5"))
	res.set("mapped.example", addrs("::ffff:127.0.0.1"))
	for i, host := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"[::1]", "[fc00::1]", "[fd00::1]", "[fe80::1]", "[::ffff:127.0.0.1]", "[::ffff:10.0.0.1]",
		"0.0.0.0", "[::]", "224.0.0.1", "[ff02::1]", "[64:ff9b::a00:1]",
		"internal.example", "metadata.example", "mixed.example", "mapped.example",
	} {
		remote := fmt.Sprintf("198.51.100.%d:1", i+1) // under the per-client limit
		rec := register(t, s, "https://"+host+":"+rc.port()+"/catalog/hooks/release", testChannel, remote)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", host, rec.Code, rec.Body.String())
		}
	}
	if n := rc.challengeCount(); n != 0 {
		t.Fatalf("refused receivers were contacted %d times", n)
	}
	if n := subCount(s); n != 0 {
		t.Fatalf("%d refused subscriptions stored", n)
	}
	if n := testutil.ToFloat64(s.metrics.registrations.WithLabelValues("refused")); n != 21 {
		t.Fatalf("refused registrations %v, want 21", n)
	}
}

func TestRegister_LoopbackOnlyBehindTheDevSwitch(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	res.set("private.test", addrs("192.168.0.10"))
	if rec := register(t, s, "http://private.test:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusForbidden {
		t.Fatalf("private address with loopback allowed: %d, want 403", rec.Code)
	}
	if rec := register(t, s, "http://127.0.0.1:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("loopback with the dev switch: %d %s", rec.Code, rec.Body.String())
	}

	strict, _ := newHookServer(t, false)
	if rec := register(t, strict, "http://127.0.0.1:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusBadRequest {
		t.Fatalf("http without the dev switch: %d, want 400", rec.Code)
	}
}

func TestRegister_RejectsBadRequests(t *testing.T) {
	s, _ := newHookServer(t, false)
	for name, body := range map[string][2]string{
		"unknown channel":  {"https://relay.example.com/h", "v9"},
		"empty channel":    {"https://relay.example.com/h", ""},
		"credentials":      {"https://user:pw@relay.example.com/h", testChannel},
		"relative":         {"/catalog/hooks/release", testChannel},
		"other scheme":     {"ftp://relay.example.com/h", testChannel},
		"does not resolve": {"https://nowhere.example/h", testChannel},
	} {
		if rec := register(t, s, body[0], body[1], clientAddr); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
}

func TestRegister_ChallengeThenRenewal(t *testing.T) {
	s, res := newHookServer(t, true)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	rc := newReceiver(t, false)
	res.set("relay.test", addrs("127.0.0.1"))
	hook := "http://relay.test:" + rc.port() + "/catalog/hooks/release"

	rec := register(t, s, hook, testChannel, clientAddr)
	var answer struct{ ExpiresAt string }
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &answer) != nil {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if want := now.Add(24 * time.Hour).Format(time.RFC3339); answer.ExpiresAt != want {
		t.Fatalf("expiresAt %s, want %s", answer.ExpiresAt, want)
	}
	rc.mu.Lock()
	challenge, host, agent := rc.challenges[0], rc.hosts[0], rc.agents[0]
	rc.mu.Unlock()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(challenge) {
		t.Fatalf("challenge %q is not 32 hex chars", challenge)
	}
	if host != "relay.test:"+rc.port() || agent != "wyolet-catalog-server/test" {
		t.Fatalf("Host %q, User-Agent %q", host, agent)
	}

	now = now.Add(6 * time.Hour)
	rec = register(t, s, hook, testChannel, clientAddr)
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &answer) != nil {
		t.Fatalf("renew: %d", rec.Code)
	}
	if want := now.Add(24 * time.Hour).Format(time.RFC3339); answer.ExpiresAt != want {
		t.Fatalf("renewed expiresAt %s, want %s", answer.ExpiresAt, want)
	}
	if n := subCount(s); n != 1 {
		t.Fatalf("%d subscriptions after a renewal, want 1", n)
	}
	if rc.challengeCount() != 2 {
		t.Fatal("a renewal must pass a fresh challenge")
	}
	for result, want := range map[string]float64{"created": 1, "renewed": 1} {
		if n := testutil.ToFloat64(s.metrics.registrations.WithLabelValues(result)); n != want {
			t.Errorf("%s registrations %v, want %v", result, n, want)
		}
	}
}

func TestRegister_HTTPSDialsPinnedAddressUnderTheHostname(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, true)
	trustReceiver(s, rc)
	res.set("example.com", addrs("127.0.0.1"))
	rec := register(t, s, "https://example.com:"+rc.port()+"/catalog/hooks/release", testChannel, clientAddr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.serverNames[0] != "example.com" || rc.hosts[0] != "example.com:"+rc.port() {
		t.Fatalf("SNI %q, Host %q; both must be the URL's host", rc.serverNames[0], rc.hosts[0])
	}
}

func TestRegister_ChallengeFailuresAreNotStored(t *testing.T) {
	for _, mode := range []string{"wrong", "error", "slow"} {
		t.Run(mode, func(t *testing.T) {
			s, res := newHookServer(t, true)
			s.hooks.timeout = 200 * time.Millisecond
			rc := newReceiver(t, false)
			rc.challenge = mode
			res.set("relay.test", addrs("127.0.0.1"))
			rec := register(t, s, "http://relay.test:"+rc.port()+"/h", testChannel, clientAddr)
			if rec.Code != http.StatusForbidden || subCount(s) != 0 {
				t.Fatalf("%d, %d stored; want 403 and none", rec.Code, subCount(s))
			}
		})
	}
}

func TestRegister_DNSRebindingStillDialsTheFirstAddress(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	// The second and every later answer points inside the network.
	res.set("rebind.test", addrs("127.0.0.1"), addrs("10.0.0.1"))
	if rec := register(t, s, "http://rebind.test:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	s.deliver(t.Context(), testChannel, "v0.1.1", s.hooks.subs.live(testChannel, s.now()))
	if rc.releaseCount() != 1 {
		t.Fatalf("nudge not delivered to the first-resolved address")
	}
	if n := res.lookups("rebind.test"); n != 1 {
		t.Fatalf("host resolved %d times, want once", n)
	}
}

func TestRegister_RateLimitPerClient(t *testing.T) {
	s, _ := newHookServer(t, false)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	bad := "https://nowhere.example/h"
	for i := range 10 {
		if rec := register(t, s, bad, testChannel, clientAddr); rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d", i+1, rec.Code)
		}
	}
	if rec := register(t, s, bad, testChannel, clientAddr); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt in a minute: %d, want 429", rec.Code)
	}
	if rec := register(t, s, bad, testChannel, "198.51.100.8:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("another client: %d, want 400", rec.Code)
	}
	now = now.Add(time.Minute)
	if rec := register(t, s, bad, testChannel, clientAddr); rec.Code != http.StatusBadRequest {
		t.Fatalf("next minute: %d, want 400", rec.Code)
	}
}

func TestRegister_RateLimitBehindTrustedProxy(t *testing.T) {
	s, _ := newHookServer(t, false)
	_, proxy, _ := net.ParseCIDR("10.0.0.0/8")
	s.cfg.TrustedProxies = []*net.IPNet{proxy}
	viaProxy := func(client string) int {
		body := strings.NewReader(`{"url":"https://nowhere.example/h","channel":"` + testChannel + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
		req.RemoteAddr = "10.0.0.2:5000"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for range 10 {
		viaProxy("203.0.113.1")
	}
	if code := viaProxy("203.0.113.1"); code != http.StatusTooManyRequests {
		t.Fatalf("11th from one client behind the proxy: %d, want 429", code)
	}
	if code := viaProxy("203.0.113.2"); code != http.StatusBadRequest {
		t.Fatalf("another client behind the same proxy: %d, want 400", code)
	}
}

func TestRegister_CapAnswers503ButRenews(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	res.set("relay.test", addrs("127.0.0.1"))
	hook := "http://relay.test:" + rc.port() + "/h"
	if rec := register(t, s, hook, testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("register: %d", rec.Code)
	}
	for i := subCount(s); i < maxSubscriptions; i++ {
		s.hooks.subs.put(subKey{url: fmt.Sprintf("https://r%d.example.com/h", i), channel: testChannel}, netip.MustParseAddr("192.0.2.1"), s.now())
	}
	if rec := register(t, s, "http://127.0.0.1:"+rc.port()+"/other", testChannel, "198.51.100.9:1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("new subscription at the cap: %d, want 503", rec.Code)
	}
	if rc.challengeCount() != 1 {
		t.Fatal("a registration over the cap was challenged")
	}
	if rec := register(t, s, hook, testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("renewal at the cap: %d, want 202", rec.Code)
	}
}

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
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
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

func TestWebhooks_Disabled(t *testing.T) {
	s, err := New(Config{RepoURL: "x", ReleasesURL: "y", MirrorDir: t.TempDir(), SyncInterval: time.Hour, CacheBytes: 1, DisableWebhooks: true})
	if err != nil {
		t.Fatal(err)
	}
	s.latest = map[string]string{testChannel: "v0.1.0"}
	if rec := register(t, s, "https://relay.example.com/h", testChannel, clientAddr); rec.Code != http.StatusNotFound {
		t.Fatalf("registration with webhooks off: %d, want 404", rec.Code)
	}
	s.nudge(t.Context(), testChannel, "v0.1.1")
}
