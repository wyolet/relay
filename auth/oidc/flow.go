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
	Nonce    string `json:"n,omitempty"`
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

// startReturn reads the ReturnParam value for Start; ok is false only when ValidateReturn refuses one, because without it an unsafe value is ignored.
func (rp *RelyingParty[U]) startReturn(r *http.Request) (string, bool) {
	raw := r.URL.Query().Get(rp.returnParamName())
	if rp.ValidateReturn == nil {
		if !safeReturnPath(raw) {
			return "", true
		}
		return raw, true
	}
	if raw == "" {
		return "", true
	}
	return rp.validReturn(r, raw)
}

// storedReturn checks the flow cookie's return target again, since the cookie is not signed; "" when there is none or it fails.
func (rp *RelyingParty[U]) storedReturn(r *http.Request, raw string) string {
	if raw == "" {
		return ""
	}
	if rp.ValidateReturn == nil {
		if !safeReturnPath(raw) {
			return ""
		}
		return raw
	}
	target, _ := rp.validReturn(r, raw)
	return target
}

func (rp *RelyingParty[U]) validReturn(r *http.Request, raw string) (string, bool) {
	target, ok := rp.ValidateReturn(r, raw)
	if !ok || target == "" || len(target) > maxReturnLen {
		return "", false
	}
	return target, true
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

// readFlowCookie decodes the flow cookie and leaves it in place.
func (rp *RelyingParty[U]) readFlowCookie(r *http.Request) (flowState, bool) {
	c, err := r.Cookie(rp.flowCookieName())
	if err != nil || c.Value == "" {
		return flowState{}, false
	}
	return decodeFlow(c.Value)
}

// takeFlowCookie reads, decodes and expires the flow cookie.
func (rp *RelyingParty[U]) takeFlowCookie(w http.ResponseWriter, r *http.Request) (flowState, bool) {
	c, err := r.Cookie(rp.flowCookieName())
	if err != nil || c.Value == "" {
		return flowState{}, false
	}
	http.SetCookie(w, &http.Cookie{
		Name: c.Name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: rp.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return decodeFlow(c.Value)
}

// decodeFlow decodes a flow cookie value. A cookie without a nonce predates nonce checking and is treated as expired, so that login restarts once.
func decodeFlow(value string) (flowState, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return flowState{}, false
	}
	var fs flowState
	if err := json.Unmarshal(raw, &fs); err != nil || fs.State == "" || fs.Verifier == "" || fs.Nonce == "" {
		return flowState{}, false
	}
	return fs, true
}
