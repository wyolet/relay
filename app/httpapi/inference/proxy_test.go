package inference

import (
	"testing"

	"github.com/wyolet/relay/app/adapter"
	apphost "github.com/wyolet/relay/app/host"
)

func TestProxyUpstreamPath_DefaultsToSpecPath(t *testing.T) {
	spec := (&adapter.Spec{UpstreamPath: "/v1/responses"}).Build()

	got := proxyUpstreamPath("/openai/v1/responses", spec, nil)

	if got != "/v1/responses" {
		t.Fatalf("path: got %q", got)
	}
}

func TestProxyUpstreamPath_HostBackendOverride(t *testing.T) {
	spec := (&adapter.Spec{UpstreamPath: "/v1/responses"}).Build()
	host := &apphost.Host{
		Spec: apphost.Spec{Backend: map[string]string{"upstreamPath": "/responses"}},
	}

	got := proxyUpstreamPath("/openai/v1/responses", spec, host)

	if got != "/responses" {
		t.Fatalf("path: got %q", got)
	}
}
