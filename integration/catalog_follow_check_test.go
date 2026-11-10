//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/storage/storagetest"
)

// TestCatalogFollowChecks covers a following relay's checks: the admin
// check-now call, never seeding an older release, failed fetches, and the
// re-check after a nudge for a tag the index doesn't name yet.
func TestCatalogFollowChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("boots relay processes")
	}
	t.Parallel()
	bin := relayBinary(t)

	// Check now: POST /api/catalog/sync (admin) runs one check and answers
	// {previous, current, changed}; on an exact-pin relay 409.
	t.Run("check now", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		r := startRelay(t, bin, "latest", env)
		r.waitMarker("v0.2.1", 60*time.Second)

		if code, body := r.do(http.MethodPost, "/api/catalog/sync", nil, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("unauthenticated sync = %d %s, want 401/403", code, body)
		}
		type syncResult struct {
			Previous string `json:"previous"`
			Current  string `json:"current"`
			Changed  *bool  `json:"changed"`
		}
		sync := func() (int, syncResult, []byte) {
			code, body := r.do(http.MethodPost, "/api/catalog/sync", nil, r.adminToken)
			var res syncResult
			_ = json.Unmarshal(body, &res)
			return code, res, body
		}
		if code, res, body := sync(); code != http.StatusOK || res.Previous != "v0.2.1" || res.Current != "v0.2.1" || res.Changed == nil || *res.Changed {
			t.Errorf("sync with nothing new = %d %s, want 200 {previous:v0.2.1,current:v0.2.1,changed:false}", code, body)
		}

		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.pushHook()
		waitFor(t, 30*time.Second, "index names v0.2.2", func() bool { return w.indexLatest() == "v0.2.2" })
		if code, res, body := sync(); code != http.StatusOK || res.Previous != "v0.2.1" || res.Current != "v0.2.2" || res.Changed == nil || !*res.Changed {
			t.Errorf("sync after a new tag = %d %s, want 200 {previous:v0.2.1,current:v0.2.2,changed:true}", code, body)
		}
		if v := r.marker().Version; v != "v0.2.2" {
			t.Errorf("marker after a changed sync = %q, want v0.2.2", v)
		}

		// An exact pin neither registers for webhooks (only with latest) nor
		// mounts the receiver.
		pinEnv := relayEnv(w, storagetest.DB(t))
		pinEnv["RELAY_CATALOG_VERSION"] = "v0.2.2"
		pinEnv["RELAY_CATALOG_WEBHOOK_URL"] = "{infer}"
		pinned := startRelay(t, bin, "pinned", pinEnv)
		pinned.waitMarker("v0.2.2", 60*time.Second)
		if code, body := pinned.do(http.MethodPost, "/api/catalog/sync", nil, pinned.adminToken); code != http.StatusConflict {
			t.Errorf("sync on an exact pin = %d %s, want 409", code, body)
		}
		if code, _ := pinned.nudge(`{"type":"release","channel":"v1alpha3","tag":"v0.2.2"}`); code != http.StatusNotFound {
			t.Errorf("release hook on an exact pin = %d, want 404 (not mounted)", code)
		}
		time.Sleep(2 * time.Second)
		if n := w.metric("catalog_server_webhook_registrations_total"); n != 0 {
			t.Errorf("an exact-pin relay with RELAY_CATALOG_WEBHOOK_URL tried %v registrations, want 0", n)
		}
	})

	// Never older applies to latest too: an index naming a release older than
	// the seeded one is skipped at boot and on a check.
	t.Run("latest never seeds an older index tag", func(t *testing.T) {
		newer := newCatalogWorld(t, worldOptions{}, "v0.2.2", modelBase, modelAdded)
		dsn := storagetest.DB(t)
		env := relayEnv(newer, dsn)
		env["RELAY_CATALOG_VERSION"] = "v0.2.2"
		first := startRelay(t, bin, "pinned-newer", env)
		first.waitMarker("v0.2.2", 60*time.Second)
		first.stop()

		older := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		env = relayEnv(older, dsn)
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		r := startRelay(t, bin, "latest-older-index", env)
		r.waitFollowing()
		if v := r.marker().Version; v != "v0.2.2" {
			t.Errorf("latest relay booted on an index naming v0.2.1 moved the marker to %q, want v0.2.2 kept", v)
		}
		code, body := r.do(http.MethodPost, "/api/catalog/sync", nil, r.adminToken)
		var res struct {
			Previous, Current string
			Changed           bool
		}
		_ = json.Unmarshal(body, &res)
		if code != http.StatusOK || res.Current != "v0.2.2" || res.Changed {
			t.Errorf("sync against an older index = %d %s, want 200 with v0.2.2 kept and changed=false", code, body)
		}
		if v := r.marker().Version; v != "v0.2.2" {
			t.Errorf("marker after a sync against an older index = %q, want v0.2.2", v)
		}
		if n := r.metric("relay_catalog_checks_total", "result=skipped_older"); n < 1 {
			t.Errorf(`relay_catalog_checks_total{result="skipped_older"} = %v, want >= 1`, n)
		}
		if !r.hasModel(modelAdded) {
			t.Errorf("model %s from v0.2.2 gone", modelAdded)
		}
	})

	// A fetch or sum failure keeps the current catalog and records lastError;
	// the check-now call reports it.
	t.Run("failed fetch keeps the catalog and records lastError", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		// A mirror that serves every release from the catalog server except a
		// corrupted v0.2.2 tarball, until repaired.
		var repaired atomic.Bool
		mirror := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			res, err := http.Get(w.http.URL + req.URL.Path)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusBadGateway)
				return
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if strings.HasSuffix(req.URL.Path, "/relay-catalog-data-v0.2.2.tar.gz") && !repaired.Load() {
				body = append([]byte("corrupt"), body...)
			}
			rw.WriteHeader(res.StatusCode)
			_, _ = rw.Write(body)
		}))
		t.Cleanup(mirror.Close)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		env["RELAY_CATALOG_URL"] = mirror.URL + "/releases/{version}/relay-catalog-data-{version}.tar.gz"
		r := startRelay(t, bin, "bad-mirror", env)
		r.waitMarker("v0.2.1", 60*time.Second)

		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.pushHook()
		waitFor(t, 30*time.Second, "index names v0.2.2", func() bool { return w.indexLatest() == "v0.2.2" })
		code, body := r.do(http.MethodPost, "/api/catalog/sync", nil, r.adminToken)
		if code != http.StatusBadGateway {
			t.Errorf("sync with a corrupt release = %d %s, want 502", code, body)
		}
		m := r.marker()
		if m.Version != "v0.2.1" {
			t.Errorf("marker after a failed fetch = %q, want v0.2.1 kept", m.Version)
		}
		if m.LastError == "" || m.CheckedAt == "" {
			t.Errorf("catalog-source after a failed check = %+v, want checkedAt and lastError set", m)
		}
		if r.hasModel(modelAdded) {
			t.Errorf("model %s present although its release failed its sum", modelAdded)
		}
		if n := r.metric("relay_catalog_checks_total", "result=failed"); n < 1 {
			t.Errorf(`relay_catalog_checks_total{result="failed"} = %v, want >= 1`, n)
		}

		// The next good check seeds the release and clears lastError.
		repaired.Store(true)
		if code, body := r.do(http.MethodPost, "/api/catalog/sync", nil, r.adminToken); code != http.StatusOK {
			t.Errorf("sync after the mirror was repaired = %d %s, want 200", code, body)
		}
		if m := r.marker(); m.Version != "v0.2.2" || m.LastError != "" {
			t.Errorf("catalog-source after a good check = %+v, want version v0.2.2 and lastError empty", m)
		}
	})

	// The nudge's tag is a hint: when the check doesn't find it in the index
	// yet, the relay checks once more about 75 s later.
	t.Run("nudge for a tag the index lacks re-checks later", func(t *testing.T) {
		requireSlow(t, "waits for the relay's re-check about 75 s after a nudge")
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		index := newIndexRecorder(t, w)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		env["RELAY_CATALOG_INDEX_URL"] = index.url()
		r := startRelay(t, bin, "recheck", env)
		r.waitMarker("v0.2.1", 60*time.Second)
		r.waitFollowing()
		checksBefore, readsBefore := r.checks(), len(index.list())
		if code, body := r.nudge(`{"type":"release","channel":"v1alpha3","tag":"v0.2.9"}`); code != http.StatusAccepted {
			t.Fatalf("nudge = %d %s", code, body)
		}
		nudged := time.Now()
		waitFor(t, 5*time.Second, "immediate index read", func() bool { return len(index.list()) > readsBefore })
		var second time.Duration
		for deadline := nudged.Add(110 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if len(index.list())-readsBefore >= 2 {
				second = time.Since(nudged)
				break
			}
		}
		for _, rd := range index.list()[readsBefore:] {
			t.Logf("index read %s after the nudge: conditional=%t status=%d", rd.at.Sub(nudged).Round(time.Millisecond), rd.conditional, rd.status)
		}
		t.Logf("checks counted after the nudge: %v", r.checks()-checksBefore)
		if second == 0 {
			t.Fatal("no second index read within 110 s of a nudge for a tag the index lacked")
		}
		if second < 60*time.Second || second > 95*time.Second {
			t.Errorf("re-check came %s after the nudge, want about 75 s", second.Round(time.Second))
		}
		if d := r.checks() - checksBefore; d != 2 {
			t.Errorf("relay_catalog_checks_total moved by %v over the nudge and its re-check, want 2", d)
		}
	})
}
