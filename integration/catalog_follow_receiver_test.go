//go:build integration

package integration_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/storage/storagetest"
)

// TestCatalogFollowReceiver covers the relay's release-webhook receiver and
// the poll that stands in for webhooks.
func TestCatalogFollowReceiver(t *testing.T) {
	if testing.Short() {
		t.Skip("boots relay processes")
	}
	t.Parallel()
	bin := relayBinary(t)

	// Receiver: type is required, unknown type → 400; body capped at 4 KiB;
	// unknown fields ignored; 202 also when debounced or another channel's;
	// at most one check per 10 s per pod.
	t.Run("receiver", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		r := startRelay(t, bin, "receiver", env)
		r.waitMarker("v0.2.1", 60*time.Second)
		r.waitFollowing()

		for name, body := range map[string]string{
			"unknown type": `{"type":"surprise","channel":"v1alpha3","tag":"v0.2.1"}`,
			"missing type": `{"channel":"v1alpha3","tag":"v0.2.1"}`,
		} {
			if code, resp := r.nudge(body); code != http.StatusBadRequest {
				t.Errorf("%s = %d %s, want 400", name, code, resp)
			}
		}
		big := `{"type":"release","channel":"v1alpha3","tag":"v0.2.1","pad":"` + strings.Repeat("x", 5000) + `"}`
		if code, resp := r.nudge(big); code < 400 || code >= 500 {
			t.Errorf("5 KB body = %d %s, want a 4xx refusal (cap 4 KiB)", code, resp)
		}

		// Another channel's release: 202, no check, no seed. It runs before the
		// debounce case so no debounce window is open.
		before := r.checks()
		if code, resp := r.nudge(`{"type":"release","channel":"v1alpha2","tag":"v9.0.0"}`); code != http.StatusAccepted {
			t.Errorf("other-channel nudge = %d %s, want 202", code, resp)
		}
		time.Sleep(2 * time.Second)
		if d := r.checks() - before; d != 0 {
			t.Errorf("other-channel nudge ran %v checks, want 0", d)
		}
		if v := r.marker().Version; v != "v0.2.1" {
			t.Errorf("marker after an other-channel nudge = %q, want v0.2.1", v)
		}

		// Debounce: 5 nudges in 1 s start one check.
		before = r.checks()
		for range 5 {
			if code, resp := r.nudge(`{"type":"release","channel":"v1alpha3","tag":"v0.2.1"}`); code != http.StatusAccepted {
				t.Errorf("own-channel nudge = %d %s, want 202", code, resp)
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(3 * time.Second)
		if d := r.checks() - before; d != 1 {
			t.Errorf("5 nudges in 1 s ran %v checks, want 1", d)
		}

		// Unknown fields are ignored.
		if code, resp := r.nudge(`{"type":"release","channel":"v1alpha3","tag":"v0.2.1","extra":{"a":1}}`); code != http.StatusAccepted {
			t.Errorf("nudge with an unknown field = %d %s, want 202", code, resp)
		}
	})

	// Poll fallback: with no webhook URL the relay registers nothing and a
	// short RELAY_CATALOG_POLL_INTERVAL picks the new tag up.
	t.Run("poll fallback", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		index := newIndexRecorder(t, w)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "2s"
		env["RELAY_CATALOG_INDEX_URL"] = index.url()
		r := startRelay(t, bin, "poller", env)
		r.waitMarker("v0.2.1", 60*time.Second)

		// One check is a conditional GET; an unchanged index answers 304.
		waitFor(t, 15*time.Second, "a conditional index read answered 304", func() bool {
			return slices.ContainsFunc(index.list(), func(r indexRead) bool { return r.conditional && r.status == http.StatusNotModified })
		})

		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.pushHook()
		pushed := time.Now()
		r.waitMarker("v0.2.2", 30*time.Second)
		t.Logf("tag push → polled seed in %s", time.Since(pushed).Round(time.Millisecond))
		waitFor(t, 10*time.Second, "model added in v0.2.2 readable", func() bool { return r.hasModel(modelAdded) })
		if n := w.metric("catalog_server_webhook_registrations_total"); n != 0 {
			t.Errorf("a relay without RELAY_CATALOG_WEBHOOK_URL registered (%v registrations)", n)
		}
		if n := r.metric("relay_catalog_checks_total", "result=unchanged"); n < 1 {
			t.Errorf(`relay_catalog_checks_total{result="unchanged"} = %v; polls of an unchanged index should count`, n)
		}
	})
}

// indexRead is one index request a relay made through an indexRecorder.
type indexRead struct {
	at          time.Time
	conditional bool // sent If-None-Match
	status      int
}

// indexRecorder proxies a catalog server's index and records each read.
type indexRecorder struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reads []indexRead
}

func newIndexRecorder(t *testing.T, w *catalogWorld) *indexRecorder {
	ir := &indexRecorder{}
	ir.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		up, _ := http.NewRequest(http.MethodGet, w.http.URL+"/index.yaml", nil)
		if v := req.Header.Get("If-None-Match"); v != "" {
			up.Header.Set("If-None-Match", v)
		}
		res, err := http.DefaultClient.Do(up)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		ir.mu.Lock()
		ir.reads = append(ir.reads, indexRead{at: time.Now(), conditional: req.Header.Get("If-None-Match") != "", status: res.StatusCode})
		ir.mu.Unlock()
		for _, h := range []string{"ETag", "Cache-Control", "Content-Type"} {
			if v := res.Header.Get(h); v != "" {
				rw.Header().Set(h, v)
			}
		}
		rw.WriteHeader(res.StatusCode)
		_, _ = io.Copy(rw, res.Body)
	}))
	t.Cleanup(ir.srv.Close)
	return ir
}

func (ir *indexRecorder) url() string { return ir.srv.URL + "/index.yaml" }

func (ir *indexRecorder) list() []indexRead {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	return append([]indexRead(nil), ir.reads...)
}
