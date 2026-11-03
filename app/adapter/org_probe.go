package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// OrgProbe is how a shape's upstream names the organization a credential belongs to: a cheap read to make, and the response headers carrying the organization.
type OrgProbe struct {
	// Path is the upstream path of a GET any credential of the shape may make and that costs nothing, such as a models list. Like CountPath it is not overridable per host.
	Path string

	// OrgHeader is the response header carrying the organization's id. The upstream sends it on every response, so live traffic can be read for it too.
	OrgHeader string

	// NameHeader is the response header carrying the organization's display name; empty when the upstream sends none.
	NameHeader string
}

// Org is the organization an upstream named for a credential.
type Org struct {
	ID   string
	Name string
}

// ErrNoOrgProbe is a probe of a shape whose upstream names no organization.
var ErrNoOrgProbe = errors.New("adapter: the upstream does not name a credential's organization")

// ErrNoOrgHeader is a probe the upstream answered without naming an organization, as an upstream serving the same shape for another provider does.
var ErrNoOrgHeader = errors.New("adapter: the upstream's answer names no organization")

// ProbeStatusError is a probe the upstream answered with a status other than 2xx.
type ProbeStatusError struct {
	Status int
}

func (e *ProbeStatusError) Error() string {
	return fmt.Sprintf("adapter: the upstream answered the organization probe with %d", e.Status)
}

// maxProbeBody bounds how much of a probe's answer is drained so its connection can be reused; only its headers are read.
const maxProbeBody = 64 << 10

// ProbeOrg asks the upstream at baseURL which organization apiKey belongs to: one GET of Org.Path, authenticated as a generation is and capped by the same sync timeout.
func (s *Spec) ProbeOrg(ctx context.Context, baseURL, apiKey string, oauth bool) (Org, error) {
	if s.Org == nil || s.orgHeader == "" || s.Org.Path == "" {
		return Org{}, ErrNoOrgProbe
	}
	a := &specAdapter{spec: s}
	callCtx, cancel := a.callContext(ctx, false)
	defer cancelIfSet(cancel)
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, baseURL+s.Org.Path, nil)
	if err != nil {
		return Org{}, err
	}
	s.setAuth(req.Header, apiKey, oauth)
	resp, err := s.client.Do(req)
	if err != nil {
		return Org{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Org{}, &ProbeStatusError{Status: resp.StatusCode}
	}
	org := Org{ID: firstValue(resp.Header, s.orgHeader), Name: firstValue(resp.Header, s.orgNameHeader)}
	if org.ID == "" {
		return Org{}, ErrNoOrgHeader
	}
	return org, nil
}

// OrgID returns the organization id a response of this shape names, "" when it names none. It indexes the header map directly, so a shape without Org pays one comparison.
func (a *specAdapter) OrgID(h http.Header) string {
	return firstValue(h, a.spec.orgHeader)
}

// firstValue is h's first value under the canonical name, "" for an empty name.
func firstValue(h http.Header, name string) string {
	if name == "" {
		return ""
	}
	if v := h[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}
