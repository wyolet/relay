package inference

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// proxyHostEntry is one row in the GET /v1/proxy/hosts response.
type proxyHostEntry struct {
	Slug        string `json:"slug"                  doc:"Host slug to send in X-WR-Upstream-Host."`
	DisplayName string `json:"displayName,omitempty" doc:"Display name."`
}

type proxyHostsOutput struct {
	Body struct {
		Object string           `json:"object" enum:"list" doc:"Always list."`
		Data   []proxyHostEntry `json:"data"   doc:"Hosts proxy mode forwards to."`
	}
}

// registerProxyHosts serves GET /v1/proxy/hosts — the list of upstream
// host slugs callers may pass in X-WR-Upstream-Host when invoking proxy
// mode. Anonymous callers see the list only when proxy mode and
// AllowUnauthenticated are both on; authed callers see it whenever
// proxy mode is enabled.
func registerProxyHosts(api huma.API, d Deps, mw huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_proxy_hosts",
		Method:      "GET",
		Path:        "/v1/proxy/hosts",
		Summary:     "List upstream hosts accepted via X-WR-Upstream-Host",
		Description: "Hosts a proxy-mode caller may name in X-WR-Upstream-Host, sending its own upstream credential. 403 while proxy mode is off; 401 for an anonymous caller unless anonymous proxy traffic is allowed.",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{401, 403, 500, 503},
	}, func(ctx context.Context, _ *struct{}) (*proxyHostsOutput, error) {
		pm, ok := proxyModeSettings(d.Catalog)
		if !ok || !pm.Enabled {
			return nil, huma.Error403Forbidden("proxy mode is not enabled")
		}
		cls := ClassificationFrom(ctx)
		if cls.Mode == ModeProxyAnonymous && !pm.AllowUnauthenticated {
			return nil, huma.Error401Unauthorized("anonymous access disabled")
		}

		snap := SnapshotFrom(ctx)
		if snap == nil {
			snap = d.Catalog.Current()
		}
		out := &proxyHostsOutput{}
		out.Body.Object = "list"
		out.Body.Data = proxyHostsFor(snap, PrincipalFrom(ctx))
		return out, nil
	})
}

// proxyHostsFor lists the hosts p may pin, by the same rule the proxy handler applies.
func proxyHostsFor(snap *appcatalog.Snapshot, p *Principal) []proxyHostEntry {
	hosts := snap.Hosts()
	out := make([]proxyHostEntry, 0, len(hosts))
	for _, h := range hosts {
		if !proxyPinAllowed(p, h) {
			continue
		}
		out = append(out, proxyHostEntry{Slug: h.Meta.Name, DisplayName: h.Meta.DisplayName})
	}
	return out
}
