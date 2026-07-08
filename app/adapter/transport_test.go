package adapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

// baseTransport unwraps the conn-tracking layer down to the tuned
// *http.Transport the pooling assertions inspect.
func baseTransport(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	ct, ok := rt.(connTrackingTransport)
	if !ok {
		t.Fatalf("transport = %T, want connTrackingTransport", rt)
	}
	tr, ok := ct.base.(*http.Transport)
	if !ok {
		t.Fatalf("tracked base = %T, want *http.Transport", ct.base)
	}
	return tr
}

// Build must give every spec a tuned, connection-pooling transport — not the
// stdlib default whose MaxIdleConnsPerHost of 2 re-dials nearly every request
// at high per-host RPS.
func TestBuild_TunedTransport(t *testing.T) {
	s := (&Spec{}).Build()

	if s.client.Timeout != defaultTimeout {
		t.Fatalf("client timeout = %v, want %v (streamed responses run minutes)", s.client.Timeout, defaultTimeout)
	}
	tr := baseTransport(t, s.client.Transport)
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d, want >= per-host cap %d", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, idleConnTimeout)
	}
	if tr.TLSHandshakeTimeout == 0 || tr.ExpectContinueTimeout == 0 {
		t.Errorf("TLSHandshakeTimeout=%v ExpectContinueTimeout=%v, both must be set",
			tr.TLSHandshakeTimeout, tr.ExpectContinueTimeout)
	}
	// Default path keeps HTTP/2 negotiation enabled.
	if tr.TLSNextProto != nil {
		t.Errorf("TLSNextProto = %v, want nil (HTTP/2 enabled on the default path)", tr.TLSNextProto)
	}
}

// UseHTTP1 disables HTTP/2 on the same tuned base — the pool tuning must
// survive the HTTP1 tweak.
func TestBuild_UseHTTP1_KeepsTuningAndDisablesH2(t *testing.T) {
	s := (&Spec{UseHTTP1: true}).Build()

	tr := baseTransport(t, s.client.Transport)
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto = %v, want non-nil empty map (HTTP/2 disabled)", tr.TLSNextProto)
	}
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Errorf("HTTP1 transport lost its per-host tuning: MaxIdleConnsPerHost = %d, want %d",
			tr.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("HTTP1 transport lost IdleConnTimeout: got %v, want %v", tr.IdleConnTimeout, idleConnTimeout)
	}
}

// The per-host cap is operator-tunable (RELAY_UPSTREAM_MAX_IDLE_PER_HOST,
// wired via SetUpstreamMaxIdleConnsPerHost); values < 1 are ignored so a zero
// config default leaves the built-in.
func TestSetUpstreamMaxIdleConnsPerHost(t *testing.T) {
	t.Cleanup(func() { maxIdleConnsPerHost = defaultMaxIdleConnsPerHost })

	SetUpstreamMaxIdleConnsPerHost(256)
	if tr := baseTransport(t, (&Spec{}).Build().client.Transport); tr.MaxIdleConnsPerHost != 256 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 256 after override", tr.MaxIdleConnsPerHost)
	}

	SetUpstreamMaxIdleConnsPerHost(0) // ignored
	if maxIdleConnsPerHost != 256 {
		t.Fatalf("maxIdleConnsPerHost = %d, want 256 (a <1 override must be ignored)", maxIdleConnsPerHost)
	}
}

// Every upstream attempt must count its connection as new (dialed) or
// reused (keep-alive pool) — the churn tripwire. Two sequential requests
// over one keep-alive connection: one new, one reused.
func TestUpstreamTransport_CountsConnectionReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	newBefore := testutil.ToFloat64(metrics.UpstreamConnections.WithLabelValues("new"))
	reusedBefore := testutil.ToFloat64(metrics.UpstreamConnections.WithLabelValues("reused"))

	client := &http.Client{Transport: NewUpstreamTransport(false)}
	for range 2 {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// Drain fully so the connection returns to the idle pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if got := testutil.ToFloat64(metrics.UpstreamConnections.WithLabelValues("new")) - newBefore; got != 1 {
		t.Errorf("new connections = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.UpstreamConnections.WithLabelValues("reused")) - reusedBefore; got != 1 {
		t.Errorf("reused connections = %v, want 1", got)
	}
}
