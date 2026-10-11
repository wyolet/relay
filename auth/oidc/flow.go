package oidc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

const (
	defaultFlowCookie = "oidc_flow"
	flowTTL           = 10 * time.Minute
)

// flowState is the per-login payload carried in the flow cookie.
type flowState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
}

func (rp *RelyingParty[U]) flowCookieName() string {
	if rp.FlowCookie == "" {
		return defaultFlowCookie
	}
	return rp.FlowCookie
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
