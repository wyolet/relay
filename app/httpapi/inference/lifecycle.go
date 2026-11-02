package inference

import (
	"context"
	"net/http"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
)

// mintLifecycle creates the per-request lifecycle Context at the inference
// entry, before routing. It carries the identity known at entry — request
// id, runner source, key hash, client IP — and a recorded timing
// anchor. Routing fills the (policy, model, host) ids later via
// applyPlanIdentity; the runner stamps the remaining timing marks. The
// caller stashes the returned Context on ctx with lifecycle.ContextWith so
// every downstream phase (routing failures included) shares this one.
//
// Tenancy + principal attribution is copied here, once: the Principal the
// auth middleware resolved plus at most three snapshot map reads for the
// project/team/service-account slugs. Post-flight observers must never
// re-resolve them — the row they name may be gone by then.
func mintLifecycle(ctx context.Context, source, clientIP string) *lifecycle.Context {
	return lifecycleFor(ctx, source, clientIP, PrincipalFrom(ctx))
}

// lifecycleFor is mintLifecycle for principal p, nil for none.
func lifecycleFor(ctx context.Context, source, clientIP string, p *Principal) *lifecycle.Context {
	lc := lifecycle.NewContext(reqid.From(ctx), source, time.Now())
	if clientIP != "" {
		lc.Metadata["client_ip"] = clientIP
	}
	if cid := reqid.ClientID(ctx); cid != "" {
		lc.Metadata[usagelog.ExtrasKeyClientRequestID] = cid
	}
	if p != nil {
		// The hash the auth middleware matched on — empty for a token, which
		// presents no key. Re-hashing the bearer here would record a hash on
		// token traffic that matches no key row.
		lc.RelayKeyHash = p.KeyHash
		applyPrincipalIdentity(lc, p)
	}
	return lc
}

// StampPrincipal copies the authenticated caller's key hash, principal and tenancy onto lc, for handlers outside Dispatch that record events against the caller. A request with no principal leaves lc untouched.
func StampPrincipal(ctx context.Context, lc *lifecycle.Context) {
	p := PrincipalFrom(ctx)
	if p == nil {
		return
	}
	lc.RelayKeyHash = p.KeyHash
	applyPrincipalIdentity(lc, p)
}

// applyPrincipalIdentity fills the tenancy + principal fields from the
// resolved Principal; the slugs come from its caller plan, which read those
// rows when it was built. A user principal carries no slug, and a token's
// project names only its id: neither is on the plan.
func applyPrincipalIdentity(lc *lifecycle.Context, p *Principal) {
	lc.CredentialKind = p.CredentialKind
	lc.CredentialID = p.CredentialID
	if p.Key != nil {
		lc.RelayKeyName = p.Key.Meta.Name
	}
	cp := p.Caller
	if cp != nil {
		lc.LaneAccountID = cp.Lane.Meta.ID
	}
	switch {
	case p.ServiceAccountID != "":
		lc.PrincipalKind = string(key.PrincipalServiceAccount)
		lc.PrincipalID = p.ServiceAccountID
		if cp != nil {
			lc.PrincipalName = cp.LaneRung().Name
		}
	case p.UserID != "":
		lc.PrincipalKind = string(key.PrincipalUser)
		lc.PrincipalID = p.UserID
	}
	if p.ProjectID != "" {
		lc.ProjectID = p.ProjectID
		if cp != nil && cp.Project().ID() == p.ProjectID {
			lc.ProjectName = cp.Project().Name
		}
	}
	if p.TeamID != "" {
		lc.TeamID = p.TeamID
		if cp != nil && cp.Team().ID() == p.TeamID {
			lc.TeamName = cp.Team().Name
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

// applyProfile records the resolved client profile, a usage dimension, and what it identifies on the request. Empty name = no profile, nothing recorded.
func applyProfile(lc *lifecycle.Context, p clientprofile.Profile, h http.Header) {
	if p.Name() == "" {
		return
	}
	lc.Metadata["client"] = p.Name()
	applyAttributionHeaders(lc, p, h)
	applySessionKey(lc, p, h)
}

// MaxAttributionValueBytes caps one recorded attribution value. The values
// ride into every usage event, so a caller cannot inflate the record.
const MaxAttributionValueBytes = 256

// applyAttributionHeaders records the client's own identifying request
// headers onto the lifecycle Context, where the usage hook picks them up as
// Event.Extras. Hot-path rule: one lookup per declared header and nothing
// allocated when they are absent.
func applyAttributionHeaders(lc *lifecycle.Context, p clientprofile.Profile, h http.Header) {
	attributor, ok := p.(clientprofile.Attributor)
	if !ok {
		return
	}
	for _, name := range attributor.AttributionHeaders() {
		v := h.Get(name)
		if v == "" {
			continue
		}
		if len(v) > MaxAttributionValueBytes {
			v = v[:MaxAttributionValueBytes]
		}
		lc.Metadata[name] = v
	}
}

// applySessionKey records the caller's conversation id under the profile-neutral key observers read. Which header carries it is the client's business and stays inside the profile.
func applySessionKey(lc *lifecycle.Context, p clientprofile.Profile, h http.Header) {
	v := sessionKeyFor(p, h)
	if v == "" {
		return
	}
	if len(v) > MaxAttributionValueBytes {
		v = v[:MaxAttributionValueBytes]
	}
	lc.Metadata[tokencount.MetadataKeySession] = v
}

// sourceCount labels count-tokens requests, which run the generation pipeline against the counting endpoint, so observers can tell them from generations.
const sourceCount = "count"

// sourceForMode maps a request mode to its runner-source label.
func sourceForMode(m Mode) string {
	if m == ModeProxyAuthed || m == ModeProxyAnonymous {
		return "proxy"
	}
	return "pipeline"
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
	d.fireFailure(ctx, &lifecycle.PostFlightEvent{ErrorKind: kind, ErrorMessage: msg})
}

// fireRouteRefusal is fireUsageFailure for a route the caller's plan refused: the row records the answer the caller got (status, code, class, message) and the model it refused. model is the caller-supplied ref.
func (d Deps) fireRouteRefusal(ctx context.Context, r *plan.Refusal, model string) {
	if lc := lifecycle.FromContext(ctx); lc != nil && d.Lifecycle != nil {
		stampRefusedModel(lc, d.refusalCatalog(ctx))
	}
	a := routeAnswer(r, model)
	d.fireFailure(ctx, &lifecycle.PostFlightEvent{Status: a.Status, ErrorKind: a.Code, Refusal: string(a.Kind), ErrorMessage: a.Message})
}

// refusalCatalog is the catalog a refused route resolved against: the caller's route table's, else the request's snapshot, else the live one (anonymous proxy traffic pins none).
func (d Deps) refusalCatalog(ctx context.Context) *appcatalog.Snapshot {
	if p := PrincipalFrom(ctx); p != nil && p.Caller != nil && p.Caller.Routes != nil {
		if snap := p.Caller.Routes.Catalog(); snap != nil {
			return snap
		}
	}
	if snap := SnapshotFrom(ctx); snap != nil {
		return snap
	}
	if d.Catalog != nil {
		return d.Catalog.Current()
	}
	return nil
}

// stampRefusedModel names on lc the catalog model a refused request asked for, as routing would have on success, so its row says which model was refused. A name no model answers keeps only the requested string; an alias naming several takes the first, the one routing tries first.
func stampRefusedModel(lc *lifecycle.Context, snap *appcatalog.Snapshot) {
	if snap == nil || lc.RequestedModel == "" || lc.ModelID != "" {
		return
	}
	res := snap.ResolveModelRef(lc.RequestedModel)
	if len(res.Models) == 0 {
		return
	}
	m := res.Models[0]
	lc.ModelID, lc.ModelName = m.Meta.ID, m.Meta.Name
	if prov, ok := snap.ProviderSlug(m.Meta.Owner.ID); ok {
		lc.ProviderName = prov
	}
}

func (d Deps) fireFailure(ctx context.Context, ev *lifecycle.PostFlightEvent) {
	if d.Lifecycle == nil {
		return
	}
	if lc := lifecycle.FromContext(ctx); lc != nil {
		finalizeDetached(d.Lifecycle, lc, ev)
	}
}

// finalizeDetached ends lc with ev off the caller's goroutine: the response is about to be written and telemetry must not hold it.
func finalizeDetached(reg *lifecycle.Registry, lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) {
	go func() {
		lc.MarkEnd()
		reg.Finalize(context.Background(), lc, ev)
	}()
}

// recordRefusal writes the usage row of a request whose credential authenticated but whose lane refused it, as a route refusal writes one; r is the request, nil records nothing. Only relay refusals are recorded, never a bad or missing credential.
func (a *Auth) recordRefusal(r *http.Request, p *Principal, refused *plan.Refusal) {
	kind := refused.Kind()
	if a == nil || a.Lifecycle == nil || r == nil || p == nil || kind == "" {
		return
	}
	ctx := r.Context()
	cls := ClassificationFrom(ctx)
	lc := lifecycleFor(ctx, sourceForMode(cls.Mode), cls.ClientIP, p)
	applyObsHeaders(lc, r.Header, false)
	applyProfile(lc, clientprofile.FromContext(ctx), r.Header)
	ans := edgeAnswer(refused)
	finalizeDetached(a.Lifecycle, lc, &lifecycle.PostFlightEvent{Status: ans.Status, ErrorKind: refused.Code, Refusal: string(kind), ErrorMessage: ans.Message})
}
