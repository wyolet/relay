package oidc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultFlowCookie  = "oidc_flow"
	defaultReturnParam = "rd"
	flowTTL            = 10 * time.Minute
	// maxReturnLen keeps the flow cookie well under the 4 KB browsers accept; a longer cookie is dropped and the login fails as expired.
	maxReturnLen = 2048
)

// flowState is the per-login payload carried in the flow cookie.
type flowState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Return   string `json:"r,omitempty"`
}

func (rp *RelyingParty[U]) flowCookieName() string {
	if rp.FlowCookie == "" {
		return defaultFlowCookie
	}
	return rp.FlowCookie
}

func (rp *RelyingParty[U]) returnParamName() string {
	if rp.ReturnParam == "" {
		return defaultReturnParam
	}
	return rp.ReturnParam
}

// safeReturnPath reports whether raw is a path on this origin, so redirecting there cannot leave the site. It also checks the percent-decoded form, in case anything between here and the browser decodes it.
func safeReturnPath(raw string) bool {
	if len(raw) > maxReturnLen || !isLocalPath(raw) {
		return false
	}
	decoded, err := url.PathUnescape(raw)
	return err == nil && isLocalPath(decoded)
}

func isLocalPath(s string) bool {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsRune(s, '\\') {
		return false
	}
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "" && u.Host == ""
}

func (rp *RelyingParty[U]) setFlowCookie(w http.ResponseWriter, fs flowState) {
	raw, _ := json.Marshal(fs)
	http.SetCookie(w, &http.Cookie{
		Name:     rp.flowCookieName(),
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/",
		MaxAge:   int(flowTTL.Seconds()),
		HttpOnly: true,
		Secure:   rp.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlowCookie reads, decodes and expires the flow cookie.
func (rp *RelyingParty[U]) takeFlowCookie(w http.ResponseWriter, r *http.Request) (flowState, bool) {
	var fs flowState
	c, err := r.Cookie(rp.flowCookieName())
	if err != nil || c.Value == "" {
		return fs, false
	}
	http.SetCookie(w, &http.Cookie{
		Name: c.Name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: rp.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return fs, false
	}
	if err := json.Unmarshal(raw, &fs); err != nil || fs.State == "" || fs.Verifier == "" {
		return flowState{}, false
	}
	return fs, true
}
