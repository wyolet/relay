//go:build !minimal

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/secret"
)

// mapStored stands in for the stored backend: what matters here is which ids the settings let a section resolve.
type mapStored map[string]string

func (m mapStored) Resolve(_ context.Context, ref secret.Ref) ([]byte, error) {
	v, ok := m[ref.ID]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(v), nil
}

// authRecorder is a fake S3 endpoint recording each request's Authorization.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auths = append(a.auths, r.Header.Get("Authorization"))
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *authRecorder) sawCredential(v string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.auths {
		if strings.Contains(h, "Credential="+v+"/") {
			return true
		}
	}
	return false
}

// A payload-logging writer must not be able to point the S3 sink or reader at
// their own endpoint with an access-key ref naming another row's stored secret
// or a relay environment variable.
func TestPayloadS3RefOutsideScopeNeverReachesEndpoint(t *testing.T) {
	t.Setenv("RELAY_COOKIE_SECURE", "false") // the fake endpoint is plain http
	rec := &authRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	const hostKeyID = "0192f3a0-0000-7000-8000-0000000000aa"
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	reg.Register(secret.KindStored, mapStored{
		hostKeyID:                    "DUMMYHOSTKEY0001",
		"payload-logging:access-key": "DUMMYSCOPEDKEY02",
	})
	t.Setenv("RELAY_MASTER_KEY", "DUMMYMASTERKEY03")
	t.Setenv("RELAY_PAYLOAD_S3_ACCESS_KEY", "DUMMYSCOPEDKEY04")
	t.Setenv("RELAY_PAYLOAD_S3_SECRET_KEY", "dummy-secret")

	cases := []struct {
		name    string
		ref     secret.Ref
		value   string
		inScope bool
	}{
		{"stored host key", secret.Ref{Kind: secret.KindStored, ID: hostKeyID}, "DUMMYHOSTKEY0001", false},
		{"env master key", secret.Ref{Kind: secret.KindEnv, Env: "RELAY_MASTER_KEY"}, "DUMMYMASTERKEY03", false},
		{"section-scoped stored", secret.Ref{Kind: secret.KindStored, ID: "payload-logging:access-key"}, "DUMMYSCOPEDKEY02", true},
		{"section-scoped env", secret.Ref{Kind: secret.KindEnv, Env: "RELAY_PAYLOAD_S3_ACCESS_KEY"}, "DUMMYSCOPEDKEY04", true},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%v", tc.name, enabled), func(t *testing.T) {
				raw, _ := json.Marshal(settings.PayloadLogging{Enabled: enabled, Backend: "s3", S3: settings.PayloadS3{
					Endpoint: endpoint, Bucket: "bkt", Region: "us-east-1", AccessKey: tc.ref,
					SecretKey: secret.Ref{Kind: secret.KindEnv, Env: "RELAY_PAYLOAD_S3_SECRET_KEY"},
				}})
				sec, _ := settings.Lookup(settings.SectionPayloadLogging)
				v, err := sec.Decode(raw)
				if err == nil {
					cfg := *v.(*settings.PayloadLogging)
					// The reader builds whatever the enabled toggle says.
					_, _ = newS3PayloadReader(context.Background(), cfg, reg)
					if enabled {
						_, _ = newS3PayloadSink(context.Background(), cfg, reg)
					}
				}
				if got := rec.sawCredential(tc.value); got != tc.inScope {
					t.Fatalf("endpoint saw the resolved value = %v, want %v (decode err: %v)", got, tc.inScope, err)
				}
			})
		}
	}
}
