package adapter

import (
	"net/http"
	"testing"
)

// Build must give every spec a tuned, connection-pooling transport — not the
// stdlib default whose MaxIdleConnsPerHost of 2 re-dials nearly every request
// at high per-host RPS.
func TestBuild_TunedTransport(t *testing.T) {
	s := (&Spec{}).Build()

	if s.client.Timeout != defaultTimeout {
		t.Fatalf("client timeout = %v, want %v (streamed responses run minutes)", s.client.Timeout, defaultTimeout)
	}
	tr, ok := s.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", s.client.Transport)
	}
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

	tr := s.client.Transport.(*http.Transport)
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
	if tr := (&Spec{}).Build().client.Transport.(*http.Transport); tr.MaxIdleConnsPerHost != 256 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 256 after override", tr.MaxIdleConnsPerHost)
	}

	SetUpstreamMaxIdleConnsPerHost(0) // ignored
	if maxIdleConnsPerHost != 256 {
		t.Fatalf("maxIdleConnsPerHost = %d, want 256 (a <1 override must be ignored)", maxIdleConnsPerHost)
	}
}
