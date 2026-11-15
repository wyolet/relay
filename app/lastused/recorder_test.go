package lastused

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/pkg/lifecycle"
)

var t0 = time.Date(2026, 11, 15, 10, 30, 42, 0, time.UTC)

func newTestRecorder(maxEntries int, now *time.Time) *Recorder {
	r := NewRecorder(maxEntries)
	r.now = func() time.Time { return *now }
	return r
}

// drained returns what r noted, as minutes, and empties it.
func drained(r *Recorder) (keys, hostKeys map[string]int64) {
	r.Drain(func(k, h map[string]int64) {
		keys, hostKeys = map[string]int64{}, map[string]int64{}
		for id, m := range k {
			keys[id] = m
		}
		for id, m := range h {
			hostKeys[id] = m
		}
	})
	return keys, hostKeys
}

func keyRequest(keyID, hostKeyRowID string) *lifecycle.Context {
	lc := lifecycle.NewContext("req", "pipeline", t0)
	lc.CredentialKind, lc.CredentialID = inference.CredentialKey, keyID
	lc.HostKeyRowID = hostKeyRowID
	return lc
}

func TestCollectNotesTheCredentialsOfAFinishedRequest(t *testing.T) {
	minute := t0.Unix() / 60
	token := lifecycle.NewContext("req", "pipeline", t0)
	token.CredentialKind, token.CredentialID, token.HostKeyRowID = inference.CredentialToken, "jti-1", "hk-1"
	for _, tc := range []struct {
		name         string
		lc           *lifecycle.Context
		ev           *lifecycle.PostFlightEvent
		wantKeys     map[string]int64
		wantHostKeys map[string]int64
	}{
		{"served", keyRequest("k-1", "hk-1"), &lifecycle.PostFlightEvent{Status: 200},
			map[string]int64{"k-1": minute}, map[string]int64{"hk-1": minute}},
		{"failed after the upstream call", keyRequest("k-1", "hk-1"), &lifecycle.PostFlightEvent{Status: 502, ErrorKind: "upstream_error"},
			map[string]int64{"k-1": minute}, map[string]int64{"hk-1": minute}},
		{"refused before any upstream call", keyRequest("k-1", ""), &lifecycle.PostFlightEvent{Status: 429, ErrorKind: "rate_limited", Refusal: "no_capacity"},
			map[string]int64{"k-1": minute}, map[string]int64{}},
		{"a token names no relay key", token, &lifecycle.PostFlightEvent{Status: 200},
			map[string]int64{}, map[string]int64{"hk-1": minute}},
		{"a keyless host's anonymous credential", keyRequest("k-1", hostkey.Anonymous("h-1", "local").Meta.ID), &lifecycle.PostFlightEvent{Status: 200},
			map[string]int64{"k-1": minute}, map[string]int64{}},
		{"no credential at all", lifecycle.NewContext("req", "proxy", t0), &lifecycle.PostFlightEvent{Status: 200},
			map[string]int64{}, map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := t0
			rec := newTestRecorder(0, &now)
			reg := lifecycle.New()
			reg.RegisterCollector(rec)
			reg.Finalize(context.Background(), tc.lc, tc.ev)
			keys, hostKeys := drained(rec)
			if diff := cmp.Diff(tc.wantKeys, keys); diff != "" {
				t.Errorf("keys (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantHostKeys, hostKeys); diff != "" {
				t.Errorf("host keys (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCredentialKeyMatchesTheInferenceEntry(t *testing.T) {
	if credentialKey != inference.CredentialKey {
		t.Errorf("credentialKey = %q, want inference.CredentialKey %q", credentialKey, inference.CredentialKey)
	}
}

func TestCollectKeepsTheLaterMinute(t *testing.T) {
	now := t0
	rec := newTestRecorder(0, &now)
	rec.Collect(keyRequest("k-1", "hk-1"))
	now = t0.Add(3 * time.Minute)
	rec.Collect(keyRequest("k-1", "hk-1"))
	rec.restore([]Entry{{ID: "k-1", At: t0}}, []Entry{{ID: "hk-1", At: t0}})
	keys, hostKeys := drained(rec)
	want := now.Unix() / 60
	if keys["k-1"] != want || hostKeys["hk-1"] != want {
		t.Errorf("minutes = %d, %d; want %d for both", keys["k-1"], hostKeys["hk-1"], want)
	}
}

func TestDrainEmptiesWhatItHandsOver(t *testing.T) {
	now := t0
	rec := newTestRecorder(0, &now)
	rec.Collect(keyRequest("k-1", "hk-1"))
	drained(rec)
	rec.Collect(keyRequest("k-2", ""))
	keys, hostKeys := drained(rec)
	if diff := cmp.Diff(map[string]int64{"k-2": t0.Unix() / 60}, keys); diff != "" {
		t.Errorf("second drain keys (-want +got):\n%s", diff)
	}
	if len(hostKeys) != 0 {
		t.Errorf("second drain host keys = %v, want none", hostKeys)
	}
}

func TestCollectDropsIdsBeyondTheCapAndCountsThem(t *testing.T) {
	now := t0
	rec := newTestRecorder(1, &now)
	before := testutil.ToFloat64(rec.lost)
	rec.Collect(keyRequest("k-1", "hk-1"))
	rec.Collect(keyRequest("k-2", "hk-2"))
	now = t0.Add(time.Minute)
	rec.Collect(keyRequest("k-1", "hk-1")) // a held id still moves forward
	keys, hostKeys := drained(rec)
	minute := now.Unix() / 60
	if diff := cmp.Diff(map[string]int64{"k-1": minute}, keys); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]int64{"hk-1": minute}, hostKeys); diff != "" {
		t.Errorf("host keys (-want +got):\n%s", diff)
	}
	if got := testutil.ToFloat64(rec.lost) - before; got != 2 {
		t.Errorf("lost = %v, want 2", got)
	}
}

// The hook runs on every request's post-flight goroutine: steady state allocates nothing.
func BenchmarkCollect(b *testing.B) {
	rec := NewRecorder(0)
	lc := keyRequest("0197a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "0197a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5c")
	rec.Collect(lc)
	b.ReportAllocs()
	for b.Loop() {
		rec.Collect(lc)
	}
}

// Across flushes too: Drain keeps both sets' storage, so ids seen before cost nothing to note again.
func BenchmarkCollectAcrossDrains(b *testing.B) {
	rec := NewRecorder(0)
	lcs := make([]*lifecycle.Context, 64)
	for i := range lcs {
		lcs[i] = keyRequest(fmt.Sprintf("k-%d", i), fmt.Sprintf("hk-%d", i%8))
	}
	noop := func(map[string]int64, map[string]int64) {}
	for range 2 {
		for _, lc := range lcs {
			rec.Collect(lc)
		}
		rec.Drain(noop)
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		rec.Collect(lcs[i%len(lcs)])
		i++
		if i%len(lcs) == 0 {
			rec.Drain(noop)
		}
	}
}

// Every post-flight goroutine shares the one lock.
func BenchmarkCollectParallel(b *testing.B) {
	rec := NewRecorder(0)
	lcs := make([]*lifecycle.Context, 64)
	for i := range lcs {
		lcs[i] = keyRequest(fmt.Sprintf("k-%d", i), fmt.Sprintf("hk-%d", i%8))
		rec.Collect(lcs[i])
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			rec.Collect(lcs[i%len(lcs)])
			i++
		}
	})
}
