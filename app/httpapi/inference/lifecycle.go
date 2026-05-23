package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
)

// buildLifecycleContext constructs the per-request lifecycle Context
// the handler hands to pipeline.Request / proxy.Request. The pipeline
// fires post-flight observers against this Context.
//
// plan may be nil (anonymous proxy or unresolved routing); fields stay
// empty and observers tolerate them. HostKeyID is left empty here —
// the runner fills it in post-flight from the actual key acquisition.
func buildLifecycleContext(ctx context.Context, source, relayKeyToken string, plan *routing.Plan) *lifecycle.Context {
	lc := lifecycle.NewContext(reqid.From(ctx), source, time.Now())
	if relayKeyToken != "" {
		sum := sha256.Sum256([]byte(relayKeyToken))
		lc.RelayKeyHash = hex.EncodeToString(sum[:])
	}
	if plan != nil {
		if plan.Policy != nil {
			lc.PolicyID = plan.Policy.Meta.ID
		}
		if plan.Model != nil {
			lc.ModelID = plan.Model.Meta.ID
		}
		if plan.Host != nil {
			lc.HostID = plan.Host.Meta.ID
		}
	}
	return lc
}

// buildProxyLifecycleContext is the proxy-mode variant. Proxy doesn't
// always have a fully-resolved Plan (anonymous mode, header-pinned host
// without policy lookup), so we accept the partial inputs the handler
// has and leave unknowns empty.
func buildProxyLifecycleContext(ctx context.Context, relayKeyToken string, host *apphost.Host, rk *relaykey.RelayKey, clientIP string) *lifecycle.Context {
	lc := lifecycle.NewContext(reqid.From(ctx), "proxy", time.Now())
	if relayKeyToken != "" {
		sum := sha256.Sum256([]byte(relayKeyToken))
		lc.RelayKeyHash = hex.EncodeToString(sum[:])
	}
	if host != nil {
		lc.HostID = host.Meta.ID
	}
	if rk != nil {
		lc.PolicyID = rk.Spec.PolicyID
	}
	if clientIP != "" {
		lc.Metadata["client_ip"] = clientIP
	}
	return lc
}
