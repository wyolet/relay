package inference

import (
	"context"
	"net/http"
	"strings"

	"github.com/wyolet/relay/pkg/httpheader"
)

// accountPathPrefix opens a base URL that names the request's lane: /a/{account}/ before any inference path.
const accountPathPrefix = "/a/"

// LaneMiddleware reads the account a request asks to run in, by precedence the X-WR-Account header, a /a/{account}/ path prefix, then the host's first label under domain (RELAY_ACCOUNT_DOMAIN; empty turns host selection off). The prefix is cut before routing, so every inference route serves under it unchanged. The credential middleware resolves the account against the caller's plans.
func LaneMiddleware(domain string) func(http.Handler) http.Handler {
	domain = strings.TrimPrefix(domain, ".")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lane := r.Header.Get(httpheader.HeaderAccount)
			pathLane, rest, prefixed := cutAccountPath(r.URL.Path)
			if lane == "" {
				lane = pathLane
			}
			if lane == "" && domain != "" {
				lane = hostAccount(r.Host, domain)
			}
			if lane == "" && !prefixed {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			if lane != "" {
				s := derivedState(ctx)
				s.lane = lane
				ctx = withState(ctx, s)
			}
			r = r.WithContext(ctx)
			if prefixed {
				u := *r.URL
				u.Path = rest
				if _, rawRest, ok := cutAccountPath(u.RawPath); ok {
					u.RawPath = rawRest
				} else {
					u.RawPath = ""
				}
				r.URL = &u
			}
			next.ServeHTTP(w, r)
		})
	}
}

// laneFrom is the account the request asked to run in, "" for the credential's default.
func laneFrom(ctx context.Context) string {
	if s := stateFrom(ctx); s != nil {
		return s.lane
	}
	return ""
}

// cutAccountPath splits "/a/{account}/rest" into the account and "/rest"; ok is false for any other path.
func cutAccountPath(path string) (account, rest string, ok bool) {
	after, found := strings.CutPrefix(path, accountPathPrefix)
	if !found {
		return "", "", false
	}
	i := strings.IndexByte(after, '/')
	if i <= 0 {
		return "", "", false
	}
	return after[:i], after[i:], true
}

// hostAccount is the account a host "<account>.<domain>" names, "" for any other host. One label only: account names are DNS labels.
func hostAccount(host, domain string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	n := len(host) - len(domain) - 1
	if n <= 0 || host[n] != '.' || !strings.EqualFold(host[n+1:], domain) {
		return ""
	}
	label := host[:n]
	if strings.IndexByte(label, '.') >= 0 {
		return ""
	}
	return strings.ToLower(label)
}
