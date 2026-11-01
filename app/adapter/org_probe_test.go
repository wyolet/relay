package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

func newOrgSpec(org *OrgProbe) *Spec {
	return (&Spec{
		Name:      adapters.OpenAI,
		Org:       org,
		Auth:      AuthStrategy{Header: "X-Api-Key", ExtraHeaders: map[string]string{"X-Version": "1"}},
		OAuthAuth: AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}).Build()
}

// The probe is one authenticated GET of the probe path; the org comes from the configured headers whatever their case.
func TestProbeOrgReadsTheOrgHeaders(t *testing.T) {
	var got *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("X-Org-Id", "org-42")
		w.Header().Set("X-Org-Name", "Acme")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer up.Close()

	s := newOrgSpec(&OrgProbe{Path: "/v1/models", OrgHeader: "x-org-id", NameHeader: "x-org-name"})
	org, err := s.ProbeOrg(context.Background(), up.URL, "sk-1", false)
	if err != nil {
		t.Fatalf("ProbeOrg: %v", err)
	}
	if org != (Org{ID: "org-42", Name: "Acme"}) {
		t.Fatalf("org = %+v", org)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v1/models" {
		t.Fatalf("probe = %s %s, want GET /v1/models", got.Method, got.URL.Path)
	}
	if got.Header.Get("X-Api-Key") != "sk-1" || got.Header.Get("X-Version") != "1" {
		t.Fatalf("probe headers = %v, want the shape's auth", got.Header)
	}

	if _, err := s.ProbeOrg(context.Background(), up.URL, "tok", true); err != nil {
		t.Fatalf("ProbeOrg oauth: %v", err)
	}
	if got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get("X-Api-Key") != "" {
		t.Fatalf("oauth probe headers = %v, want the OAuth variant", got.Header)
	}
}

func TestProbeOrgFailures(t *testing.T) {
	status, withHeader := http.StatusOK, false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if withHeader {
			w.Header().Set("X-Org-Id", "org-42")
		}
		w.WriteHeader(status)
	}))
	defer up.Close()
	ctx := context.Background()

	if _, err := newOrgSpec(nil).ProbeOrg(ctx, up.URL, "sk", false); !errors.Is(err, ErrNoOrgProbe) {
		t.Fatalf("no probe: err = %v, want ErrNoOrgProbe", err)
	}

	s := newOrgSpec(&OrgProbe{Path: "/v1/models", OrgHeader: "X-Org-Id"})
	if _, err := s.ProbeOrg(ctx, up.URL, "sk", false); !errors.Is(err, ErrNoOrgHeader) {
		t.Fatalf("no header: err = %v, want ErrNoOrgHeader", err)
	}

	status, withHeader = http.StatusUnauthorized, true
	_, err := s.ProbeOrg(ctx, up.URL, "sk", false)
	var se *ProbeStatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("401: err = %v, want ProbeStatusError 401", err)
	}
}

// Live responses are read for the org on the request path, so the read must not allocate.
func TestOrgIDReadsTheHeaderWithoutAllocating(t *testing.T) {
	h := http.Header{"X-Org-Id": {"org-42"}}
	a := newOrgSpec(&OrgProbe{Path: "/v1/models", OrgHeader: "x-org-id"}).PipelineAdapter().(*specAdapter)
	if got := a.OrgID(h); got != "org-42" {
		t.Fatalf("OrgID = %q", got)
	}
	plain := newOrgSpec(nil).PipelineAdapter().(*specAdapter)
	if got := plain.OrgID(h); got != "" {
		t.Fatalf("OrgID without a probe = %q, want empty", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = a.OrgID(h) }); n != 0 {
		t.Fatalf("OrgID allocs = %v, want 0", n)
	}
}
