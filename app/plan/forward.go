package plan

import (
	"context"
	"log/slog"
	"strings"

	"github.com/wyolet/relay/pkg/engine"
)

// Forwarder feeds the catalog's NOTIFY stream to an engine the plans are registered on: each notification for a kind the plans read becomes an engine event, and every (re)attach of the listener a resync, since notifications sent while it was detached never arrive.
type Forwarder struct{ Engine *engine.Engine }

// Event forwards one catalog notification; kinds the plans do not read are ignored.
func (f Forwarder) Event(kind, op, id string) {
	ref, ok := NotifyRef(kind, id)
	if !ok {
		return
	}
	ev := engine.Event{Ref: ref, Op: engine.OpUpsert}
	if op == "delete" {
		ev.Op = engine.OpDelete
	}
	f.Engine.Notify(ev)
}

// Attached resyncs the engine against its sources.
func (f Forwarder) Attached(ctx context.Context) {
	if err := f.Engine.Resync(ctx); err != nil {
		slog.Error("plans: resync after the listener attached failed", "err", err)
	}
}

// NotifyRef maps a catalog notification to the engine row it names.
func NotifyRef(kind, id string) (engine.Ref, bool) {
	switch kind {
	case "provider", "host", "model", "pricing", "policy", "policybinding", "ratelimit",
		"budget", "pool", "account", "team", "project", "serviceaccount", "user":
		return engine.Ref{Kind: engine.Kind(kind), ID: id}, true
	case "hostkey":
		return engine.Ref{Kind: KindCredential, ID: id}, true
	case "relaykey":
		return engine.Ref{Kind: KindKey, ID: id}, true
	case "hostbinding":
		return engine.Ref{Kind: KindBinding, ID: id}, true
	case "overlay":
		// The overlay payload id is "<kind>|<resource id>".
		k, rid, ok := strings.Cut(id, "|")
		if !ok {
			return engine.Ref{}, false
		}
		return engine.Ref{Kind: KindOverlay, ID: OverlayID(k, rid)}, true
	}
	return engine.Ref{}, false
}
