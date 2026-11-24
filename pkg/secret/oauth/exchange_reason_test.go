package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/wyolet/relay/pkg/publicaddr"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
)

// A failed exchange reads as a short classified reason; the network error text, which can name resolved addresses, never shows.
func TestExchangeReason(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 443}, Err: errors.New("connection refused")}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{unlistedHostError{host: "token.example"}, `"token.example" is not in RELAY_OAUTH_TOKEN_HOSTS`},
		{fmt.Errorf("oauth: token request failed: %w", &net.OpError{Op: "dial", Err: publicaddr.ErrRefused, Addr: dialErr.Addr}), "does not resolve to a public address"},
		{fmt.Errorf("oauth: token request failed: %w", errRedirect), "redirect"},
		{sdkoauth.ErrNoAccessToken, "no access token"},
		{errShortLifetime, "under 15 minutes"},
		{&oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant", ErrorDescription: "said by the endpoint"}, "answered HTTP 400 (invalid_grant)"},
		{fmt.Errorf("oauth: token request failed: %w", context.DeadlineExceeded), "timed out"},
		{fmt.Errorf("oauth: token request failed: %w", dialErr), "could not be reached"},
	} {
		got := exchangeError{tc.err}.Error()
		if !strings.Contains(got, tc.want) || strings.Contains(got, "10.1.2.3") || strings.Contains(got, "said by the endpoint") {
			t.Errorf("%v: reason %q, want it to contain %q and no address or endpoint text", tc.err, got, tc.want)
		}
	}
}
