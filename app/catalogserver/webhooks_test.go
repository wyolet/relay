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
		wantAnswer(t, host, answer{Code: http.StatusForbidden, Body: "url resolves to a refused address\n"}, register(t, s, "https://"+host+":"+rc.port()+"/catalog/hooks/release", testChannel, remote))
	}
	if n := rc.challengeCount(); n != 0 {
		t.Fatalf("refused receivers were contacted %d times", n)
	}
	if n := subCount(s); n != 0 {
		t.Fatalf("%d refused subscriptions stored", n)
	}
	wantCount(t, "refused registrations", s.metrics.registrations.WithLabelValues("refused"), 21)
}

func TestRegister_LoopbackOnlyBehindTheDevSwitch(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	res.set("private.test", addrs("192.168.0.10"))
	wantAnswer(t, "private address with loopback allowed", answer{Code: http.StatusForbidden, Body: "url resolves to a refused address\n"}, register(t, s, "http://private.test:"+rc.port()+"/h", testChannel, clientAddr))
	if rec := register(t, s, "http://127.0.0.1:"+rc.port()+"/h", testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("loopback with the dev switch: %d %s", rec.Code, rec.Body.String())
	}

	strict, _ := newHookServer(t, false)
	wantAnswer(t, "http without the dev switch", answer{Code: http.StatusBadRequest, Body: "invalid url: url must be https\n"}, register(t, strict, "http://127.0.0.1:"+rc.port()+"/h", testChannel, clientAddr))
	if n := rc.challengeCount(); n != 1 {
		t.Fatalf("receiver challenged %d times, want once (the accepted registration)", n)
	}
}

func TestRegister_RejectsBadRequests(t *testing.T) {
	s, _ := newHookServer(t, false)
	body := func(hookURL, channel string) string {
		b, _ := json.Marshal(registerRequest{URL: hookURL, Channel: channel})
		return string(b)
	}
	cases := []struct{ name, body, want string }{
		{"not JSON", "url=https://relay.example.com/h", `body must be JSON {"url", "channel"}`},
		{"body over the limit", `{"url":"` + strings.Repeat("a", maxHookBody) + `"}`, `body must be JSON {"url", "channel"}`},
		{"unknown channel", body("https://relay.example.com/h", "v9"), "unknown channel"},
		{"empty channel", body("https://relay.example.com/h", ""), "unknown channel"},
		{"credentials", body("https://user:pw@relay.example.com/h", testChannel), "invalid url: url must not carry credentials"},
		{"relative", body("/catalog/hooks/release", testChannel), "invalid url: url must be https"},
		{"other scheme", body("ftp://relay.example.com/h", testChannel), "invalid url: url must be https"},
		{"no host", body("https:///h", testChannel), "invalid url: url has no host"},
		{"unparsable", body("https://relay.example.com/%zz", testChannel), `invalid url: parse "https://relay.example.com/%zz": invalid URL escape "%zz"`},
		{"too long", body("https://relay.example.com/"+strings.Repeat("a", maxHookURL), testChannel), "invalid url: url longer than 2048 bytes"},
		{"does not resolve", body("https://nowhere.example/h", testChannel), "url host does not resolve"},
	}
	for i, c := range cases {
		wantAnswer(t, c.name, answer{Code: http.StatusBadRequest, Body: c.want + "\n"}, registerRaw(s, c.body, fmt.Sprintf("198.51.100.%d:1", i+1))) // under the per-client limit
	}
	wantCount(t, "invalid registrations", s.metrics.registrations.WithLabelValues("invalid"), float64(len(cases)))
	if n := subCount(s); n != 0 {
		t.Fatalf("%d rejected subscriptions stored", n)
	}
}

func TestRegister_ChallengeThenRenewal(t *testing.T) {
	s, res := newHookServer(t, true)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	rc := newReceiver(t, false)
	res.set("relay.test", addrs("127.0.0.1"))
	hook := "http://relay.test:" + rc.port() + "/catalog/hooks/release"

	accepted := func(expiresAt string) answer {
		return answer{Code: http.StatusAccepted, Body: `{"expiresAt":"` + expiresAt + `"}` + "\n", Header: map[string]string{"Content-Type": "application/json"}}
	}
	wantAnswer(t, "register", accepted("2026-11-11T12:00:00Z"), register(t, s, hook, testChannel, clientAddr))
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
	// The fragment is no part of the receiver's identity.
	wantAnswer(t, "renew", accepted("2026-11-11T18:00:00Z"), register(t, s, hook+"#again", testChannel, clientAddr))
	if n := subCount(s); n != 1 {
		t.Fatalf("%d subscriptions after a renewal, want 1", n)
	}
	if rc.challengeCount() != 2 {
		t.Fatal("a renewal must pass a fresh challenge")
	}
	for result, want := range map[string]float64{"created": 1, "renewed": 1} {
		wantCount(t, result+" registrations", s.metrics.registrations.WithLabelValues(result), want)
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
			wantAnswer(t, "answer", answer{Code: http.StatusForbidden, Body: "challenge failed\n"}, register(t, s, "http://relay.test:"+rc.port()+"/h", testChannel, clientAddr))
			if n, failed := subCount(s), testutil.ToFloat64(s.metrics.registrations.WithLabelValues("challenge_failed")); n != 0 || failed != 1 {
				t.Fatalf("%d stored, %v counted as challenge_failed; want 0, 1", n, failed)
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
	want := answer{Code: http.StatusTooManyRequests, Body: "too many registrations; retry in a minute\n"}
	wantAnswer(t, "11th attempt in a minute", want, register(t, s, bad, testChannel, clientAddr))
	if rec := register(t, s, bad, testChannel, "198.51.100.8:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("another client: %d, want 400", rec.Code)
	}
	now = now.Add(time.Minute - time.Second)
	if rec := register(t, s, bad, testChannel, clientAddr); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a second before the minute ends: %d, want 429", rec.Code)
	}
	now = now.Add(time.Second)
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
	wantAnswer(t, "new subscription at the cap", answer{Code: http.StatusServiceUnavailable, Body: "subscription limit reached\n"}, register(t, s, "http://127.0.0.1:"+rc.port()+"/other", testChannel, "198.51.100.9:1"))
	if rc.challengeCount() != 1 {
		t.Fatal("a registration over the cap was challenged")
	}
	if rec := register(t, s, hook, testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("renewal at the cap: %d, want 202", rec.Code)
	}
}

func TestWebhooks_Disabled(t *testing.T) {
	s := must(New(Config{RepoURL: "x", ReleasesURL: "y", MirrorDir: t.TempDir(), SyncInterval: time.Hour, CacheBytes: 1, DisableWebhooks: true}))(t)
	s.latest = map[string]string{testChannel: "v0.1.0"}
	if rec := register(t, s, "https://relay.example.com/h", testChannel, clientAddr); rec.Code != http.StatusNotFound {
		t.Fatalf("registration with webhooks off: %d, want 404", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/github", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repository hook with webhooks off: %d, want 404", rec.Code)
	}
	s.nudge(t.Context(), testChannel, "v0.1.1")
}
