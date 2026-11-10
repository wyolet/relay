//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/storage/storagetest"
)

const (
	modelBase  = "claude-haiku-4-5"
	modelAdded = "claude-sonnet-4"
)

// TestCatalogFollow boots the relay binary against a real catalog server and
// checks how a relay on RELAY_CATALOG_VERSION=latest follows new releases
// through webhooks. The catalog-follow tests run in parallel with each other;
// their subtests run in order, since several share one relay or database.
func TestCatalogFollow(t *testing.T) {
	if testing.Short() {
		t.Skip("boots relay processes")
	}
	t.Parallel()
	bin := relayBinary(t)

	// Boot on latest seeds the channel's latest tag and records it in the
	// catalog-source marker.
	t.Run("boot on latest seeds the channel latest", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{}, "v0.2.1", modelBase)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		r := startRelay(t, bin, "latest", env)

		m := r.waitMarker("v0.2.1", 60*time.Second)
		if m.SeededAt == "" {
			t.Errorf("catalog-source seededAt is empty after the boot seed: %+v", m)
		}
		if !r.hasModel(modelBase) {
			t.Errorf("model %s missing after the boot seed", modelBase)
		}
		waitFor(t, 10*time.Second, `relay_catalog_seeded_version{version="v0.2.1"} = 1`, func() bool {
			return r.metric("relay_catalog_seeded_version", "version=v0.2.1") == 1
		})
	})

	t.Run("webhook", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		spy := newHookSpy(t)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		env["RELAY_CATALOG_WEBHOOK_URL"] = "{infer}"
		r := startRelay(t, bin, "follower", env)
		r.waitMarker("v0.2.1", 60*time.Second)

		// Registration: the relay registers <base>/catalog/hooks/release for its
		// channel with the catalog server once the catalog is loaded.
		t.Run("relay registers with the server", func(t *testing.T) {
			waitFor(t, 30*time.Second, "server holds the relay's subscription", func() bool {
				return w.metric("catalog_server_webhook_subscriptions", "channel="+followChannel) == 1
			})
			if n := w.metric("catalog_server_webhook_registrations_total", "result=created"); n != 1 {
				t.Errorf(`registrations_total{result="created"} = %v, want 1`, n)
			}
		})

		// Challenge: body {"type":"challenge","challenge":"<32 hex>"} → 200
		// {"challenge":"<same value>"}.
		t.Run("receiver echoes a challenge", func(t *testing.T) {
			const ch = "0123456789abcdef0123456789abcdef"
			code, body := r.nudge(`{"type":"challenge","challenge":"` + ch + `"}`)
			if code != http.StatusOK {
				t.Fatalf("challenge = %d %s, want 200", code, body)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil || got["challenge"] != ch {
				t.Errorf("challenge answer = %s, want {\"challenge\":%q}", body, ch)
			}
		})

		// Nudge: on a new channel-latest tag the server POSTs {"type":"release",
		// "channel","tag"} with UA wyolet-catalog-server/<version>; the relay
		// checks within seconds and seeds with no restart.
		t.Run("new tag is nudged and seeded without a restart", func(t *testing.T) {
			if code, body := w.register(spy.url(), followChannel); code != http.StatusAccepted {
				t.Fatalf("register spy = %d %s", code, body)
			}
			pid := r.cmd.Process.Pid
			before := r.checks()

			w.publishTag("v0.2.2", modelBase, modelAdded)
			pushed := time.Now()
			w.pushHook()

			waitFor(t, 30*time.Second, "spy receives the release nudge", func() bool { return len(spy.releaseRequests()) > 0 })
			nudge := spy.releaseRequests()[0]
			if nudge.body["type"] != "release" || nudge.body["channel"] != followChannel || nudge.body["tag"] != "v0.2.2" {
				t.Errorf("nudge body = %v, want type=release channel=%s tag=v0.2.2", nudge.body, followChannel)
			}
			if !strings.HasPrefix(nudge.userAgent, "wyolet-catalog-server/") {
				t.Errorf("nudge User-Agent = %q, want wyolet-catalog-server/<version>", nudge.userAgent)
			}
			if !strings.HasPrefix(nudge.contentType, "application/json") {
				t.Errorf("nudge Content-Type = %q, want application/json", nudge.contentType)
			}

			r.waitMarker("v0.2.2", 30*time.Second)
			t.Logf("tag push → relay seeded in %s (nudge after %s)", time.Since(pushed).Round(time.Millisecond), nudge.at.Sub(pushed).Round(time.Millisecond))
			waitFor(t, 10*time.Second, "model added in v0.2.2 readable", func() bool { return r.hasModel(modelAdded) })
			if !r.running() || r.cmd.Process.Pid != pid {
				t.Error("relay restarted while following")
			}
			if n := r.metric("relay_catalog_checks_total", "result=updated"); n != 1 {
				t.Errorf(`relay_catalog_checks_total{result="updated"} = %v, want 1`, n)
			}
			if d := r.checks() - before; d < 1 {
				t.Errorf("no check counted after the nudge (delta %v)", d)
			}
			waitFor(t, 10*time.Second, "seeded_version gauge moves to v0.2.2", func() bool {
				return r.metric("relay_catalog_seeded_version", "version=v0.2.2") == 1
			})
			m := r.marker()
			if m.CheckedAt == "" {
				t.Errorf("catalog-source checkedAt empty after a running check: %+v", m)
			}
			if m.LastError != "" {
				t.Errorf("catalog-source lastError = %q after a good check", m.LastError)
			}
		})

		// Never older: a relay with an exact older pin on the same database
		// boots, logs the skip and keeps the newer catalog; with
		// RELAY_CATALOG_ALLOW_DOWNGRADE=1 it seeds the older one.
		t.Run("an older pin never drags the catalog back", func(t *testing.T) {
			if r.marker().Version != "v0.2.2" {
				t.Skip("needs the v0.2.2 seed of the previous case")
			}
			pinEnv := relayEnv(w, env["RELAY_PG_DSN"])
			pinEnv["RELAY_CATALOG_VERSION"] = "v0.2.1"
			pinned := startRelay(t, bin, "pinned-older", pinEnv)
			const logLine = "catalog: pinned version older than the seeded one; keeping it"
			waitFor(t, 60*time.Second, "pinned relay logs the kept catalog", func() bool {
				return strings.Contains(pinned.logs.String(), logLine)
			})
			if v := pinned.marker().Version; v != "v0.2.2" {
				t.Errorf("after an older pin's boot the marker is %q, want v0.2.2 kept", v)
			}
			if !pinned.hasModel(modelAdded) {
				t.Errorf("model %s from v0.2.2 gone after an older pin's boot", modelAdded)
			}
			pinned.stop()

			pinEnv["RELAY_CATALOG_ALLOW_DOWNGRADE"] = "1"
			down := startRelay(t, bin, "downgrade", pinEnv)
			down.waitMarker("v0.2.1", 60*time.Second)
		})
	})

	// One seeder: two relays on latest sharing one database get the same
	// nudge; the seed runs once under the advisory lock.
	t.Run("two followers on one database seed once", func(t *testing.T) {
		w := newCatalogWorld(t, worldOptions{allowLoopback: true}, "v0.2.1", modelBase)
		env := relayEnv(w, storagetest.DB(t))
		env["RELAY_CATALOG_VERSION"] = "latest"
		env["RELAY_CATALOG_POLL_INTERVAL"] = "0"
		env["RELAY_CATALOG_WEBHOOK_URL"] = "{infer}"
		a := startRelay(t, bin, "a", env)
		b := startRelay(t, bin, "b", env)
		a.waitMarker("v0.2.1", 60*time.Second)
		waitFor(t, 30*time.Second, "both relays registered", func() bool {
			return w.metric("catalog_server_webhook_subscriptions", "channel="+followChannel) == 2
		})
		ca, cb := a.checks(), b.checks()
		seededBefore := a.marker().SeededAt

		w.publishTag("v0.2.2", modelBase, modelAdded)
		w.pushHook()
		m := a.waitMarker("v0.2.2", 30*time.Second)
		waitFor(t, 15*time.Second, "both relays ran a check", func() bool {
			return a.checks() > ca && b.checks() > cb
		})
		time.Sleep(3 * time.Second)

		results := func(p *relayProc) string {
			return "updated=" + metricString(p.metric("relay_catalog_checks_total", "result=updated")) +
				" unchanged=" + metricString(p.metric("relay_catalog_checks_total", "result=unchanged")) +
				" skipped_locked=" + metricString(p.metric("relay_catalog_checks_total", "result=skipped_locked"))
		}
		t.Logf("relay a checks: %s; relay b checks: %s", results(a), results(b))
		updated := a.metric("relay_catalog_checks_total", "result=updated") + b.metric("relay_catalog_checks_total", "result=updated")
		if updated != 1 {
			t.Errorf(`relay_catalog_checks_total{result="updated"} summed over both relays = %v, want exactly 1`, updated)
		}
		if after := a.marker(); after.SeededAt != m.SeededAt || after.SeededAt == seededBefore {
			t.Errorf("seededAt moved more than once: before %q, first v0.2.2 read %q, now %q", seededBefore, m.SeededAt, after.SeededAt)
		}
		// The seeded-version gauge is documented as "pods disagreeing for more
		// than a few seconds means one is not receiving catalog changes".
		for _, p := range []*relayProc{a, b} {
			if v := p.metric("relay_catalog_seeded_version", "version=v0.2.2"); v != 1 {
				t.Errorf(`relay %s: relay_catalog_seeded_version{version="v0.2.2"} = %v, want 1 (%s)`, p.name, v, results(p))
			}
			if !p.hasModel(modelAdded) {
				t.Errorf("relay %s doesn't read model %s", p.name, modelAdded)
			}
		}
	})
}
