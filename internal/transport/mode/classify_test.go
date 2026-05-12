package mode_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/internal/transport/mode"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		headers     map[string]string
		remoteAddr  string
		wantMode    mode.Mode
		wantErr     error
		wantRelay   string
		wantProv    string
		wantIP      string
	}{
		{
			name:      "no header + relay key via Authorization",
			headers:   map[string]string{"Authorization": "Bearer wr_mykey"},
			wantMode:  mode.ModeNormal,
			wantRelay: "wr_mykey",
		},
		{
			name:      "No-Proxy header + relay key via X-WR-API-Key",
			headers:   map[string]string{"X-WR-Proxy-Mode": "No-Proxy", "X-WR-API-Key": "relay-k"},
			wantMode:  mode.ModeNormal,
			wantRelay: "relay-k",
		},
		{
			name:      "No-Proxy header + relay key via x-api-key",
			headers:   map[string]string{"X-WR-Proxy-Mode": "No-Proxy", "x-api-key": "relay-k2"},
			wantMode:  mode.ModeNormal,
			wantRelay: "relay-k2",
		},
		{
			name:     "no header no key — still normal (key empty; auth enforces it)",
			headers:  map[string]string{},
			wantMode: mode.ModeNormal,
		},
		{
			name:      "Proxy mode + relay key + provider key → ProxyAuthed",
			headers:   map[string]string{"X-WR-Proxy-Mode": "Proxy", "X-WR-API-Key": "wr_relay", "Authorization": "Bearer sk-prov"},
			wantMode:  mode.ModeProxyAuthed,
			wantRelay: "wr_relay",
			wantProv:  "sk-prov",
		},
		{
			name:     "Proxy mode + provider key only → ProxyAnonymous",
			headers:  map[string]string{"X-WR-Proxy-Mode": "Proxy", "Authorization": "Bearer sk-anon"},
			wantMode: mode.ModeProxyAnonymous,
			wantProv: "sk-anon",
		},
		{
			name:    "Proxy mode without Authorization → error",
			headers: map[string]string{"X-WR-Proxy-Mode": "Proxy"},
			wantErr: mode.ErrMissingProviderKey,
		},
		{
			name:    "Proxy mode + non-Bearer Authorization → error",
			headers: map[string]string{"X-WR-Proxy-Mode": "Proxy", "Authorization": "Basic dXNlcjpwYXNz"},
			wantErr: mode.ErrMissingProviderKey,
		},
		{
			name:    "invalid proxy mode value → error",
			headers: map[string]string{"X-WR-Proxy-Mode": "passthrough"},
			wantErr: mode.ErrInvalidProxyModeHeader,
		},
		{
			name:    "case-sensitive header value — 'proxy' is invalid",
			headers: map[string]string{"X-WR-Proxy-Mode": "proxy"},
			wantErr: mode.ErrInvalidProxyModeHeader,
		},
		{
			name:      "normal mode: X-WR-API-Key wins over x-api-key and Authorization",
			headers:   map[string]string{"X-WR-API-Key": "first", "x-api-key": "second", "Authorization": "Bearer third"},
			wantMode:  mode.ModeNormal,
			wantRelay: "first",
		},
		{
			name:      "normal mode: x-api-key wins over Authorization when no X-WR-API-Key",
			headers:   map[string]string{"x-api-key": "second", "Authorization": "Bearer third"},
			wantMode:  mode.ModeNormal,
			wantRelay: "second",
		},
		{
			name:       "RemoteAddr used as IP when no trusted proxies",
			headers:    map[string]string{},
			remoteAddr: "10.0.0.5:55000",
			wantMode:   mode.ModeNormal,
			wantIP:     "10.0.0.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.remoteAddr != "" {
				req.RemoteAddr = tc.remoteAddr
			} else {
				req.RemoteAddr = "127.0.0.1:12345"
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			got, err := mode.Classify(req)
			if tc.wantErr != nil {
				if err != tc.wantErr {
					t.Fatalf("error: got %v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Mode != tc.wantMode {
				t.Errorf("Mode: got %v want %v", got.Mode, tc.wantMode)
			}
			if got.RelayKey != tc.wantRelay {
				t.Errorf("RelayKey: got %q want %q", got.RelayKey, tc.wantRelay)
			}
			if got.ProviderKey != tc.wantProv {
				t.Errorf("ProviderKey: got %q want %q", got.ProviderKey, tc.wantProv)
			}
			if tc.wantIP != "" && got.ClientIP != tc.wantIP {
				t.Errorf("ClientIP: got %q want %q", got.ClientIP, tc.wantIP)
			}
		})
	}
}
