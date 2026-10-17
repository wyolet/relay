package control

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/auth/oidc"
	"github.com/wyolet/relay/pkg/httpmw"
)

// validateReturn accepts an absolute http(s) URL on an origin this control plane is reached at or sends browsers to: the callback's, PostLoginURL's, a CORS-allowed one, or the request's own.
func (od *oidcDeps) validateReturn(r *http.Request, raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	origin, ok := originOf(u)
	if !ok {
		return "", false
	}
	cfg := od.cfg()
	allowed := make([]string, 0, 3+len(od.allowOrigins))
	allowed = append(allowed, cfg.RedirectURL, cfg.PostLoginURL, requestOrigin(r, od.trustedProxies))
	allowed = append(allowed, od.allowOrigins...)
	for _, a := range allowed {
		au, err := url.Parse(a)
		if err != nil {
			continue
		}
		if ao, ok := originOf(au); ok && ao == origin {
			return u.String(), true
		}
	}
	return "", false
}

// originOf is u's scheme, lowercased host and port, the scheme's default port filled in so equal origins compare equal; false unless u is absolute http(s) with a host.
func originOf(u *url.URL) (string, bool) {
	port := u.Port()
	switch u.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port), true
}

// requestOrigin is the origin the request arrived at. X-Forwarded-Proto counts only from a trusted proxy, or any client could claim https.
func requestOrigin(r *http.Request, trusted []*net.IPNet) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if httpmw.FromTrustedProxy(r, trusted) {
		proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
		if strings.EqualFold(strings.TrimSpace(proto), "https") {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

// renderOIDCFailure sends the browser back to the login's return target with ?error=<code>, or answers the API error envelope without one. The provider's own error text never reaches the response.
func renderOIDCFailure(w http.ResponseWriter, r *http.Request, f *oidc.Failure) {
	code := f.Code
	if code == "" {
		code = oidc.CodeLoginFailed
	}
	if f.Return != "" {
		if u, err := url.Parse(f.Return); err == nil {
			q := u.Query()
			q.Set("error", code)
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
	}
	msg := f.Message
	switch code {
	case oidc.CodeIDPError:
		msg = "login failed at identity provider"
	case oidc.CodeExchangeFailed:
		msg = "code exchange with the identity provider failed"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	_ = json.NewEncoder(w).Encode(httpapi.APIError{
		Err: httpapi.APIErrorBody{Type: "authentication_error", Code: code, Message: msg},
	})
}
