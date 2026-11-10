//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var challengeHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestCatalogServerWebhooks checks the catalog server's side of release
// webhooks against receivers on loopback: registration answers, the
// challenge, nudges and their retries.
func TestCatalogServerWebhooks(t *testing.T) {
	if testing.Short() {
		t.Skip("runs catalog servers over git mirrors")
	}
	t.Parallel()

	// 403 for a host on a refused address (loopback, private, link-local,
	// CGNAT, ULA) without the loopback allowance; 400 for a URL that isn't
	// https or doesn't resolve.
	t.Run("refused receiver addresses", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		spy := newHookSpy(t)
		for _, u := range []string{
			"https://127.0.0.1:8443/catalog/hooks/release",
			"https://localhost:8443/catalog/hooks/release",
			"https://10.1.2.3/catalog/hooks/release",
			"https://192.168.1.10/catalog/hooks/release",
			"https://172.16.0.5/catalog/hooks/release",
			"https://169.254.169.254/catalog/hooks/release",
			"https://100.64.0.1/catalog/hooks/release",
			"https://[::1]/catalog/hooks/release",
			"https://[fd00::1]/catalog/hooks/release",
			"https://[fe80::1]/catalog/hooks/release",
			spy.url(), // http on loopback, without the allowance
		} {
			code, body := w.register(u, followChannel)
			want := http.StatusForbidden
			if strings.HasPrefix(u, "http://") {
				want = http.StatusBadRequest // not https
			}
			if code != want && !(strings.HasPrefix(u, "http://") && code == http.StatusForbidden) {
				t.Errorf("register %s = %d %s, want %d", u, code, strings.TrimSpace(body), want)
			}
		}
		for _, u := range []string{
			"http://93.184.216.34/catalog/hooks/release",
			"https://user:pw@93.184.216.34/catalog/hooks/release",
			"https://does-not-resolve.invalid/catalog/hooks/release",
			"not a url",
		} {
			if code, body := w.register(u, followChannel); code != http.StatusBadRequest {
				t.Errorf("register %q = %d %s, want 400", u, code, strings.TrimSpace(body))
			}
		}
		if n := len(spy.challengeRequests()); n != 0 {
			t.Errorf("a refused receiver got %d challenge requests", n)
		}
		if n := w.metric("catalog_server_webhook_subscriptions"); n != 0 {
			t.Errorf("subscriptions after refusals = %v, want 0", n)
		}
	})

	t.Run("registration", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)

		// The loopback allowance admits loopback only; private ranges stay refused.
		if code, body := w.register("http://10.1.2.3/catalog/hooks/release", followChannel); code != http.StatusForbidden {
			t.Errorf("register a private http address with the loopback allowance = %d %s, want 403", code, body)
		}

		spy := newHookSpy(t)
		if code, body := w.register(spy.url(), "v9nope"); code != http.StatusBadRequest {
			t.Errorf("register for an unknown channel = %d %s, want 400", code, body)
		}
		res, err := http.Post(w.http.URL+"/v1/webhooks", "application/json", strings.NewReader(`{"url":`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("register with a malformed body = %d, want 400", res.StatusCode)
		}

		// 202 {"expiresAt": RFC 3339} once the challenge passed; the challenge
		// is {"type":"challenge","challenge":"<32 hex>"} as JSON with the
		// server's User-Agent.
		code, body := w.register(spy.url(), followChannel)
		if code != http.StatusAccepted {
			t.Fatalf("register = %d %s, want 202", code, body)
		}
		var ok struct {
			ExpiresAt string `json:"expiresAt"`
		}
		if err := json.Unmarshal([]byte(body), &ok); err != nil {
			t.Fatalf("register answer %s: %v", body, err)
		}
		exp, err := time.Parse(time.RFC3339, ok.ExpiresAt)
		if err != nil {
			t.Errorf("expiresAt %q is not RFC 3339", ok.ExpiresAt)
		} else if d := time.Until(exp); d < 23*time.Hour || d > 25*time.Hour {
			t.Errorf("expiresAt is %s away, want about 24 h", d.Round(time.Minute))
		}
		ch := spy.challengeRequests()
		if len(ch) != 1 {
			t.Fatalf("challenge requests = %d, want 1", len(ch))
		}
		if ch[0].body["type"] != "challenge" || !challengeHex.MatchString(fmt.Sprint(ch[0].body["challenge"])) {
			t.Errorf("challenge body = %v, want type=challenge and 32 hex chars", ch[0].body)
		}
		if !strings.HasPrefix(ch[0].contentType, "application/json") {
			t.Errorf("challenge Content-Type = %q, want application/json", ch[0].contentType)
		}
		if !strings.HasPrefix(ch[0].userAgent, "wyolet-catalog-server/") {
			t.Errorf("challenge User-Agent = %q, want wyolet-catalog-server/<version>", ch[0].userAgent)
		}

		// Re-registering the same (url, channel) renews and answers the same way.
		if code, body := w.register(spy.url(), followChannel); code != http.StatusAccepted {
			t.Errorf("re-register = %d %s, want 202", code, body)
		}
		if n := w.metric("catalog_server_webhook_subscriptions", "channel="+followChannel); n != 1 {
			t.Errorf("subscriptions after a renewal = %v, want 1", n)
		}

		// A receiver that fails the challenge is refused with 403 and not stored.
		for name, mod := range map[string]func(string) (int, string){
			"wrong echo": func(string) (int, string) { return http.StatusOK, `{"challenge":"0123"}` },
			"500":        func(string) (int, string) { return http.StatusInternalServerError, "boom" },
			"202 echo":   func(c string) (int, string) { return http.StatusAccepted, `{"challenge":"` + c + `"}` },
			"slow echo": func(c string) (int, string) {
				time.Sleep(6 * time.Second)
				return http.StatusOK, `{"challenge":"` + c + `"}`
			},
		} {
			bad := newHookSpy(t)
			bad.challengeMod = mod
			if code, body := w.register(bad.url(), followChannel); code != http.StatusForbidden {
				t.Errorf("register a receiver with a %s challenge = %d %s, want 403", name, code, body)
			}
		}
		if n := w.metric("catalog_server_webhook_subscriptions", "channel="+followChannel); n != 1 {
			t.Errorf("subscriptions after failed challenges = %v, want 1 (only the good receiver)", n)
		}
	})

	// 429 beyond 10 registrations per minute from one client.
	t.Run("registration rate limit", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		spy := newHookSpy(t)
		const client = "203.0.113.9"
		var codes []int
		for i := range 11 {
			code, _ := w.registerFrom(client, fmt.Sprintf("%s/r%d", spy.srv.URL, i), followChannel)
			codes = append(codes, code)
		}
		for i, c := range codes[:10] {
			if c != http.StatusAccepted {
				t.Errorf("registration %d = %d, want 202", i+1, c)
			}
		}
		if codes[10] != http.StatusTooManyRequests {
			t.Errorf("11th registration in a minute = %d, want 429", codes[10])
		}
		if code, body := w.registerFrom("203.0.113.10", spy.srv.URL+"/other", followChannel); code != http.StatusAccepted {
			t.Errorf("registration from another client = %d %s, want 202 (the limit is per client)", code, body)
		}
	})

	// Nudges go to every subscription of the channel for the channel's new
	// latest tag only; a receiver that failed its challenge never hears of one.
	t.Run("nudges", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		good, refused := newHookSpy(t), newHookSpy(t)
		refused.challengeMod = func(string) (int, string) { return http.StatusOK, `{"challenge":"nope"}` }
		if code, body := w.register(good.url(), followChannel); code != http.StatusAccepted {
			t.Fatalf("register = %d %s", code, body)
		}
		if code, _ := w.register(refused.url(), followChannel); code != http.StatusForbidden {
			t.Fatalf("register refused receiver = %d, want 403", code)
		}

		// v0.2.0 is tagged after v0.2.2 and lands in the same sync; it never
		// becomes the channel's latest, so only v0.2.2 is nudged.
		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.publishTag("v0.2.0", modelBase)
		w.pushHook()
		waitFor(t, 30*time.Second, "receiver nudged", func() bool { return len(good.releaseRequests()) > 0 })
		waitFor(t, 30*time.Second, "server serves v0.2.0", func() bool {
			res, err := http.Get(w.http.URL + "/releases/v0.2.0/catalog.json.gz.sha256")
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == http.StatusOK
		})
		time.Sleep(time.Second)
		if latest := w.indexLatest(); latest != "v0.2.2" {
			t.Errorf("index latest = %q, want v0.2.2", latest)
		}
		got := good.releaseRequests()
		if len(got) != 1 || got[0].body["tag"] != "v0.2.2" {
			bodies := make([]any, len(got))
			for i, r := range got {
				bodies[i] = r.body
			}
			t.Errorf("receiver nudges = %v, want exactly one for v0.2.2", bodies)
		}
		if n := len(refused.releaseRequests()); n != 0 {
			t.Errorf("receiver that failed its challenge got %d nudges, want 0", n)
		}
		if n := w.metric("catalog_server_webhook_nudges_total", "result=delivered"); n != 1 {
			t.Errorf(`catalog_server_webhook_nudges_total{result="delivered"} = %v, want 1`, n)
		}
	})

	// A receiver answering non-2xx, or slower than the 5 s timeout, is retried
	// after 10 s (then 60 s and 300 s, not waited for here).
	t.Run("retries", func(t *testing.T) {
		requireSlow(t, "waits for real nudge retry timers (about 25 s)")
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		good, failing, slow, refused := newHookSpy(t), newHookSpy(t), newHookSpy(t), newHookSpy(t)
		failing.releaseCode = http.StatusInternalServerError
		slow.releaseDelay = 7 * time.Second
		refused.challengeMod = func(string) (int, string) { return http.StatusOK, `{"challenge":"nope"}` }
		for _, s := range []*hookSpy{good, failing, slow} {
			if code, body := w.register(s.url(), followChannel); code != http.StatusAccepted {
				t.Fatalf("register = %d %s", code, body)
			}
		}
		if code, _ := w.register(refused.url(), followChannel); code != http.StatusForbidden {
			t.Fatalf("register refused receiver = %d, want 403", code)
		}

		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.pushHook()
		waitFor(t, 30*time.Second, "both receivers nudged", func() bool {
			return len(good.releaseRequests()) > 0 && len(failing.releaseRequests()) > 0
		})
		first := failing.releaseRequests()[0].at
		waitFor(t, 25*time.Second, "first retry to the failing receiver", func() bool { return len(failing.releaseRequests()) >= 2 })
		gap := failing.releaseRequests()[1].at.Sub(first)
		if gap < 9*time.Second || gap > 12*time.Second {
			t.Errorf("first retry came %s after the failed nudge, want about 10 s (another receiver of the same tag timed out at 5 s)", gap.Round(time.Millisecond))
		}
		for _, r := range failing.releaseRequests() {
			if r.body["type"] != "release" || r.body["tag"] != "v0.2.2" || r.body["channel"] != followChannel {
				t.Errorf("retry body = %v", r.body)
			}
		}
		if n := len(good.releaseRequests()); n != 1 {
			t.Errorf("delivered receiver got %d nudges for one tag, want 1", n)
		}
		if n := len(refused.releaseRequests()); n != 0 {
			t.Errorf("receiver that failed its challenge got %d nudges, want 0", n)
		}
		if n := w.metric("catalog_server_webhook_nudges_total", "result=failed"); n < 2 {
			t.Errorf(`catalog_server_webhook_nudges_total{result="failed"} = %v, want >= 2`, n)
		}
		if n := w.metric("catalog_server_webhook_nudges_total", "result=delivered"); n != 1 {
			t.Errorf(`catalog_server_webhook_nudges_total{result="delivered"} = %v, want 1`, n)
		}

		// A receiver slower than the 5 s timeout counts as failed and is retried.
		waitFor(t, 25*time.Second, "retry to the slow receiver", func() bool { return len(slow.releaseRequests()) >= 2 })
		sr := slow.releaseRequests()
		if gap := sr[1].at.Sub(sr[0].at); gap < 13*time.Second || gap > 20*time.Second {
			t.Errorf("slow receiver retried %s after the first attempt, want about 5 s timeout + 10 s", gap.Round(time.Millisecond))
		}
	})
}

// TestCatalogIndexReadiness publishes a tag the way release CI does (tag
// first, release files a little later) and requires that the index never
// names a tag whose files don't all serve.
func TestCatalogIndexReadiness(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a catalog server over a git mirror")
	}
	t.Parallel()
	w := newCatalogWorld(t, worldOptions{syncInterval: 300 * time.Millisecond, disableHooks: true}, "v0.2.1", modelBase)
	const tag = "v0.2.2"
	files := []string{
		"relay-catalog-data-" + tag + ".tar.gz.sha256",
		"catalog.json.gz.sha256",
		"relay-catalog-data-" + tag + ".tar.gz",
		"catalog.json.gz",
	}
	get := func(name string) (int, string) {
		res, err := http.Get(w.http.URL + "/releases/" + tag + "/" + name)
		if err != nil {
			return 0, ""
		}
		res.Body.Close()
		return res.StatusCode, res.Header.Get("Cache-Control")
	}

	var (
		mu        sync.Mutex
		violation []string
		seen      bool
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if w.indexLatest() == tag {
				mu.Lock()
				seen = true
				mu.Unlock()
				for _, f := range files {
					if code, _ := get(f); code != http.StatusOK {
						mu.Lock()
						violation = append(violation, fmt.Sprintf("index names %s but %s answers %d", tag, f, code))
						mu.Unlock()
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	// The tag lands before its files, as on GitHub. A request for its sum in
	// that window may 404 but must not be cached.
	w.repo.commitTag(tag, catalogTreeV3(modelBase, modelAdded))
	early := make(chan [2]string, 1)
	go func() {
		code, cc := get(files[0])
		early <- [2]string{fmt.Sprint(code), cc}
	}()
	time.Sleep(2 * time.Second)
	if w.indexLatest() == tag {
		t.Errorf("index names %s before its release files are published", tag)
	}
	data, cat := w.repo.releaseFiles(tag)
	w.releases.publish(tag, data, cat)

	waitFor(t, 30*time.Second, "index names "+tag, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen
	})
	time.Sleep(2 * time.Second)
	close(stop)
	<-done
	for _, v := range violation {
		t.Error(v)
	}

	select {
	case e := <-early:
		t.Logf("sum requested before the files were published answered %s (Cache-Control %q)", e[0], e[1])
		if e[0] == "404" && !strings.Contains(e[1], "no-store") {
			t.Errorf("404 for an unpublished release file carries Cache-Control %q, want no-store", e[1])
		}
	case <-time.After(25 * time.Second):
		t.Error("early sum request still waiting after 25 s (the server waits at most 20 s)")
	}
	for _, f := range files {
		if code, _ := get(f); code != http.StatusOK {
			t.Errorf("after the tag became ready %s answers %d, want 200", f, code)
		}
	}
}
