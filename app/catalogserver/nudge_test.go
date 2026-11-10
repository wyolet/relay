package catalogserver

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// subscribe registers rc for testChannel through the real endpoint.
func subscribe(t *testing.T, s *Server, res *fakeResolver, rc *receiver) subKey {
	t.Helper()
	res.set("relay.test", addrs("127.0.0.1"))
	hook := "http://relay.test:" + rc.port() + "/catalog/hooks/release"
	if rec := register(t, s, hook, testChannel, clientAddr); rec.Code != http.StatusAccepted {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	return subKey{url: hook, channel: testChannel}
}

func failedTags(s *Server, k subKey) (int, bool) {
	s.hooks.subs.mu.Lock()
	defer s.hooks.subs.mu.Unlock()
	sub := s.hooks.subs.subs[k]
	if sub == nil {
		return 0, false
	}
	return sub.failedTags, true
}

func TestNudge_RetriesThreeTimesThenGivesUp(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	key := subscribe(t, s, res, rc)
	rc.failReleases = -1

	s.deliver(t.Context(), testChannel, "v0.1.1", s.hooks.subs.live(testChannel, s.now()))
	if n := rc.releaseCount(); n != 4 {
		t.Fatalf("%d attempts, want 1 + 3 retries", n)
	}
	if n, ok := failedTags(s, key); !ok || n != 1 {
		t.Fatalf("failed tags %d (kept %v), want 1", n, ok)
	}
	for result, want := range map[string]float64{"failed": 4, "gave_up": 1, "delivered": 0} {
		if n := testutil.ToFloat64(s.metrics.nudges.WithLabelValues(result)); n != want {
			t.Errorf("%s nudges %v, want %v", result, n, want)
		}
	}
	if got := nudgeRetryDelays; len(got) != 3 || got[0] != 10*time.Second || got[1] != time.Minute || got[2] != 5*time.Minute {
		t.Fatalf("production retry delays %v", got)
	}
}

func TestNudge_RetrySucceeds(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	key := subscribe(t, s, res, rc)
	rc.failReleases = 2

	s.deliver(t.Context(), testChannel, "v0.1.1", s.hooks.subs.live(testChannel, s.now()))
	if n := rc.releaseCount(); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}
	rc.mu.Lock()
	last := rc.releases[2]
	rc.mu.Unlock()
	if last["type"] != "release" || last["channel"] != testChannel || last["tag"] != "v0.1.1" {
		t.Fatalf("nudge body %v", last)
	}
	if n, _ := failedTags(s, key); n != 0 {
		t.Fatalf("failed tags %d after a delivery, want 0", n)
	}
}

func TestNudge_RemovesReceiverAfterThreeFailedTags(t *testing.T) {
	s, res := newHookServer(t, true)
	rc := newReceiver(t, false)
	key := subscribe(t, s, res, rc)
	deliver := func(tag string) {
		s.deliver(t.Context(), testChannel, tag, s.hooks.subs.live(testChannel, s.now()))
	}

	rc.failReleases = -1
	deliver("v0.1.1")
	deliver("v0.1.2")
	rc.failReleases = 0
	deliver("v0.1.3") // a success in between resets the count
	if n, _ := failedTags(s, key); n != 0 {
		t.Fatalf("failed tags %d after a delivery", n)
	}
	rc.failReleases = -1
	deliver("v0.1.4")
	deliver("v0.1.5")
	if _, ok := failedTags(s, key); !ok {
		t.Fatal("removed after two failed tags")
	}
	deliver("v0.1.6")
	if _, ok := failedTags(s, key); ok {
		t.Fatal("kept after three failed tags in a row")
	}
	before := rc.releaseCount()
	deliver("v0.1.7")
	if rc.releaseCount() != before {
		t.Fatal("a removed receiver was nudged")
	}
}

func TestNudge_ConcurrencyIsBounded(t *testing.T) {
	s, _ := newHookServer(t, true)
	rc := newReceiver(t, false)
	var subs []subscription
	for i := range 3 * nudgeSenders {
		k := subKey{url: fmt.Sprintf("%s/h%d", rc.srv.URL, i), channel: testChannel}
		s.hooks.subs.put(k, netip.MustParseAddr("127.0.0.1"), s.now())
		subs = append(subs, subscription{subKey: k, addr: netip.MustParseAddr("127.0.0.1")})
	}
	inFlight, peak := 0, 0
	rc.onRelease = func(string) {
		rc.mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		rc.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		rc.mu.Lock()
		inFlight--
		rc.mu.Unlock()
	}
	s.deliver(t.Context(), testChannel, "v0.1.1", subs)
	if rc.releaseCount() != len(subs) {
		t.Fatalf("%d of %d delivered", rc.releaseCount(), len(subs))
	}
	if peak > nudgeSenders || peak < 2 {
		t.Fatalf("peak concurrency %d, want 2..%d", peak, nudgeSenders)
	}
}

func TestNudge_OnlyForReadyTagsAndNotAtStartup(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	s := newTestServer(t, src, rel, 64<<20)
	res := newFakeResolver()
	useReceiverPolicy(s, receiverPolicy{resolver: res, allowLoopback: true}, nil)
	rc := newReceiver(t, false)
	// A subscription held from before a restart: the first sync must not nudge it.
	s.hooks.subs.put(subKey{url: rc.srv.URL + "/h", channel: testChannel}, netip.MustParseAddr("127.0.0.1"), s.now())
	ctx := context.Background()

	s.sync(ctx)
	s.deliveries.Wait()
	if rc.releaseCount() != 0 {
		t.Fatal("nudged at startup for a tag the server already had")
	}

	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), time.Now().Add(-time.Minute).Truncate(time.Second))
	src.tag("v0.1.1", false)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publish("v0.1.1", CatalogAsset, cat1, cat1)
	s.sync(ctx)
	s.deliveries.Wait()
	if rc.releaseCount() != 0 {
		t.Fatal("nudged before the data file's sum was published")
	}

	// Every file the nudge names must already serve when it arrives.
	served := map[string]int{}
	rc.onRelease = func(tag string) {
		for _, name := range []string{CatalogAsset, CatalogAsset + ".sha256", DataAsset(tag), DataAsset(tag) + ".sha256"} {
			served[name] = get(t, s, "/releases/"+tag+"/"+name).Code
		}
		served["index"] = http.StatusNotFound
		if indexLatest(t, s)[testChannel] == tag {
			served["index"] = http.StatusOK
		}
	}
	rel.publish("v0.1.1", DataAsset("v0.1.1"), data1, data1)
	s.sync(ctx)
	s.deliveries.Wait()
	if rc.releaseCount() != 1 {
		t.Fatalf("%d nudges once v0.1.1 is ready, want 1", rc.releaseCount())
	}
	for name, code := range served {
		if code != http.StatusOK {
			t.Errorf("%s answered %d when the nudge arrived", name, code)
		}
	}
	s.sync(ctx)
	s.deliveries.Wait()
	if rc.releaseCount() != 1 {
		t.Fatal("nudged again for an unchanged latest")
	}
}

// putReceiver subscribes rc for testChannel without a challenge.
func putReceiver(s *Server, rc *receiver) subscription {
	k := subKey{url: rc.srv.URL + "/h", channel: testChannel}
	addr := netip.MustParseAddr("127.0.0.1")
	s.hooks.subs.put(k, addr, s.now())
	return subscription{subKey: k, addr: addr}
}

func TestNudge_RetryWaitsFromItsOwnFailure(t *testing.T) {
	s, _ := newHookServer(t, true)
	s.hooks.timeout = 400 * time.Millisecond
	delays := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond}
	s.hooks.retryDelays = delays

	slow := newReceiver(t, false)
	slowFirst := true
	slow.onRelease = func(string) {
		slow.mu.Lock()
		first := slowFirst
		slowFirst = false
		slow.mu.Unlock()
		if first {
			time.Sleep(600 * time.Millisecond) // past the timeout
		}
	}
	failing := newReceiver(t, false)
	failing.failReleases = -1
	var attempts []time.Time
	failing.onRelease = func(string) {
		failing.mu.Lock()
		attempts = append(attempts, time.Now())
		failing.mu.Unlock()
	}
	failingSub := putReceiver(s, failing)

	s.deliver(t.Context(), testChannel, "v0.1.1", []subscription{putReceiver(s, slow), failingSub})

	failing.mu.Lock()
	defer failing.mu.Unlock()
	if len(attempts) != 1+len(delays) {
		t.Fatalf("%d attempts at the failing receiver, want 1 + %d retries", len(attempts), len(delays))
	}
	// Counted from the slow receiver's timeout, the first retry would come
	// after timeout + delay; each must follow its own failure instead.
	for i, d := range delays {
		gap := attempts[i+1].Sub(attempts[i])
		if gap < d || gap > d+250*time.Millisecond {
			t.Errorf("retry %d came %v after the failed attempt, want about %v", i+1, gap, d)
		}
	}
	if n, ok := failedTags(s, failingSub.subKey); !ok || n != 1 {
		t.Fatalf("failing receiver: failed tags %d (kept %v), want 1", n, ok)
	}
	if n := slow.releaseCount(); n != 2 {
		t.Fatalf("%d attempts at the slow receiver, want 2", n)
	}
	if n := testutil.ToFloat64(s.metrics.nudges.WithLabelValues("gave_up")); n != 1 {
		t.Fatalf("gave_up %v, want 1", n)
	}
}

func TestNudge_NewerTagSupersedesPendingRetries(t *testing.T) {
	s, _ := newHookServer(t, true)
	s.hooks.retryDelays = []time.Duration{500 * time.Millisecond, time.Millisecond, time.Millisecond}
	rc := newReceiver(t, false)
	rc.failReleases = 1
	sub := putReceiver(s, rc)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.deliver(t.Context(), testChannel, "v0.1.1", []subscription{sub})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for rc.releaseCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first nudge never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.deliver(t.Context(), testChannel, "v0.1.2", []subscription{sub})
	<-done

	rc.mu.Lock()
	var tags []string
	for _, r := range rc.releases {
		tags = append(tags, r["tag"])
	}
	rc.mu.Unlock()
	if len(tags) != 2 || tags[0] != "v0.1.1" || tags[1] != "v0.1.2" {
		t.Fatalf("nudged tags %v, want [v0.1.1 v0.1.2]", tags)
	}
	for result, want := range map[string]float64{"superseded": 1, "gave_up": 0, "delivered": 1} {
		if n := testutil.ToFloat64(s.metrics.nudges.WithLabelValues(result)); n != want {
			t.Errorf("%s nudges %v, want %v", result, n, want)
		}
	}
	if n, _ := failedTags(s, sub.subKey); n != 0 {
		t.Fatalf("failed tags %d, want 0", n)
	}
}
