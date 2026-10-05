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
	"github.com/wyolet/relay/pkg/secret"
)

type memSecretRow struct {
	ct, nonce []byte
	ver       int32
}

type memSecretStore struct {
	mu sync.Mutex
	m  map[string]memSecretRow
}

func (s *memSecretStore) Get(_ context.Context, id string) ([]byte, []byte, int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return nil, nil, 0, errors.New("not found")
	}
	return r.ct, r.nonce, r.ver, nil
}

func (s *memSecretStore) Put(_ context.Context, id string, ct, nonce []byte, ver int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = memSecretRow{ct: ct, nonce: nonce, ver: ver}
	return nil
}

func (s *memSecretStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
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

	masterKey := []byte("0123456789abcdef0123456789abcdef")
	stored := secret.NewStoredResolver(&memSecretStore{m: map[string]memSecretRow{}}, masterKey, 1)
	const hostKeyID = "0192f3a0-0000-7000-8000-0000000000aa"
	if _, err := stored.Create(context.Background(), hostKeyID, []byte("DUMMYHOSTKEY0001")); err != nil {
		t.Fatal(err)
	}
	if _, err := stored.Create(context.Background(), "payload-logging:access-key", []byte("DUMMYSCOPEDKEY02")); err != nil {
		t.Fatal(err)
	}
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	reg.Register(secret.KindStored, stored)
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
