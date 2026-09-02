package inference

import (
	"context"
	"net/http"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
)

// mintLifecycle creates the per-request lifecycle Context at the inference
// entry, before routing. It carries the identity known at entry — request
// id, runner source, key hash, client IP — and a stamped timing
// anchor. Routing fills the (policy, model, host) ids later via
// applyPlanIdentity; the runner stamps the remaining timing marks. The
// caller stashes the returned Context on ctx with lifecycle.ContextWith so
// every downstream phase (routing failures included) shares this one.
//
// Tenancy + principal attribution is copied here, once: the Principal the
// auth middleware resolved plus at most three snapshot map reads for the
// project/team/service-account slugs. Post-flight observers must never
// re-resolve them — the row they name may be gone by then.
func mintLifecycle(ctx context.Context, cat *appcatalog.Catalog, source, clientIP string) *lifecycle.Context {
	lc := lifecycle.NewContext(reqid.From(ctx), source, time.Now())
	if clientIP != "" {
		lc.Metadata["client_ip"] = clientIP
	}
	if p := PrincipalFrom(ctx); p != nil {
		// The hash the auth middleware matched on — empty for a token, which
		// presents no key. Re-hashing the bearer here would stamp a hash on
		// token traffic that matches no key row.
		lc.RelayKeyHash = p.KeyHash
		// The snapshot the credential resolved against, so the slugs named
		// here describe the same rows the principal was built from.
		snap := SnapshotFrom(ctx)
		if snap == nil && cat != nil {
			snap = cat.Current()
		}
		if snap != nil {
			applyPrincipalIdentity(lc, snap, p)
		}
	}
	return lc
}

// applyPrincipalIdentity fills the tenancy + principal fields from the
// resolved Principal, resolving the three slugs against snapshot rows the
// ids already point at. A user principal carries no slug: users are not in
// the snapshot and looking one up would be a Postgres call on the hot path.
func applyPrincipalIdentity(lc *lifecycle.Context, snap *appcatalog.Snapshot, p *Principal) {
	lc.CredentialKind = p.CredentialKind
	lc.CredentialID = p.CredentialID
	switch {
	case p.ServiceAccountID != "":
		lc.PrincipalKind = string(key.PrincipalServiceAccount)
		lc.PrincipalID = p.ServiceAccountID
		if sa, ok := snap.ServiceAccount(p.ServiceAccountID); ok {
			lc.PrincipalName = sa.Meta.Name
		}
	case p.UserID != "":
		lc.PrincipalKind = string(key.PrincipalUser)
		lc.PrincipalID = p.UserID
	}
	if p.ProjectID != "" {
		lc.ProjectID = p.ProjectID
		if proj, ok := snap.Project(p.ProjectID); ok {
			lc.ProjectName = proj.Meta.Name
		}
	}
	if p.TeamID != "" {
		lc.TeamID = p.TeamID
		if t, ok := snap.Team(p.TeamID); ok {
			lc.TeamName = t.Meta.Name
		}
	}
}

// applyObsHeaders captures the inbound observability headers onto the
// lifecycle Context. Hot-path rule: O(1) header lookups and one string
// copy — the tags JSON is parsed post-flight (usagelog hook), never here.
// Both headers are inside the X-WR-* strip denylist, so they never reach
// the upstream.
func applyObsHeaders(lc *lifecycle.Context, h http.Header, trustEventTime bool) {
	if trustEventTime {
		if v := h.Get(httpheader.HeaderEventTime); v != "" {
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				lc.EventTime = t
			}
		}
	}
	if v := h.Get(httpheader.HeaderRequestTags); v != "" && len(v) <= usagelog.MaxTagsHeaderBytes {
		lc.Metadata[usagelog.MetadataKeyRequestTags] = v
	}
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
		lc.PolicyName = plan.Policy.Meta.Name
	}
	if plan.Model != nil {
		lc.ModelID = plan.Model.Meta.ID
		lc.ModelName = plan.Model.Meta.Name
	}
	if plan.Host != nil {
		lc.HostID = plan.Host.Meta.ID
		lc.HostName = plan.Host.Meta.Name
	}
	if plan.Provider != "" {
		lc.ProviderName = plan.Provider
	}
	if plan.Pricing != nil {
		lc.PricingID = plan.Pricing.Meta.ID
		lc.PricingName = plan.Pricing.Meta.Name
	}
	if plan.ResolvedVia != "" {
		lc.Metadata["resolved_via"] = plan.ResolvedVia
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
		d.Lifecycle.Finalize(fctx, lc, &lifecycle.PostFlightEvent{
			ErrorKind:    kind,
			ErrorMessage: msg,
		})
	}()
}
