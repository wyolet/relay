package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
)

// mintLifecycle creates the per-request lifecycle Context at the inference
// entry, before routing. It carries the identity known at entry — request
// id, runner source, relay-key hash, client IP — and a stamped timing
// anchor. Routing fills the (policy, model, host) ids later via
// applyPlanIdentity; the runner stamps the remaining timing marks. The
// caller stashes the returned Context on ctx with lifecycle.ContextWith so
// every downstream phase (routing failures included) shares this one.
func mintLifecycle(ctx context.Context, source, relayKeyToken, clientIP string) *lifecycle.Context {
	lc := lifecycle.NewContext(reqid.From(ctx), source, time.Now())
	if relayKeyToken != "" {
		sum := sha256.Sum256([]byte(relayKeyToken))
		lc.RelayKeyHash = hex.EncodeToString(sum[:])
	}
	if clientIP != "" {
		lc.Metadata["client_ip"] = clientIP
	}
	return lc
}

// sourceForMode maps a request mode to its runner-source label.
func sourceForMode(m Mode) string {
	if m == ModeProxyAuthed || m == ModeProxyAnonymous {
		return "proxy"
	}
	return "pipeline"
}

// applyPlanIdentity fills the routing-identity fields once a Plan resolves.
// Nil-safe in both arguments so partial-resolution paths (anonymous proxy,
// header-pinned host) can call it unconditionally.
func applyPlanIdentity(lc *lifecycle.Context, plan *routing.Plan) {
	if lc == nil || plan == nil {
		return
	}
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

// fireUsageFailure emits a failure post-flight observer event for a request
// that failed before any runner was invoked — routing rejections, proxy
// gating, translate errors. Runner-stage failures (no_keys, upstream_error,
// rate_limited) are fired by the runner itself, so callers must only use
// this for pre-runner failures to avoid a double emit.
//
// Runs in its own goroutine: the caller is about to write the error
// response and telemetry must not block it. Status is the upstream HTTP
// status, which is 0 here because upstream was never reached — ErrorKind
// carries the reason.
func (d Deps) fireUsageFailure(ctx context.Context, kind, msg string) {
	if d.Lifecycle == nil {
		return
	}
	lc := lifecycle.FromContext(ctx)
	if lc == nil {
		return
	}
	go func() {
		lc.MarkEnd()
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.Lifecycle.FirePostFlight(fctx, lc, &lifecycle.PostFlightEvent{
			ErrorKind:    kind,
			ErrorMessage: msg,
		})
	}()
}
