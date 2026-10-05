package otlpreceiver

import (
	"context"
	"log/slog"
	"strings"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// record queues one usage event per call not recorded before, then stores the content of the calls that carry any. It reports false when the usage queue could not take them all: the client must then send the export again, and whatever was queued this time is skipped as a duplicate.
func (h *Handler) record(ctx context.Context, sig signal, snap *appcatalog.Snapshot, reporter *lifecycle.Context, from origin, calls []reported, now time.Time) bool {
	queue := h.opts.Usage
	// Refused before anything is marked when the export cannot fit. An export larger than the receiver's whole share never fits, so it is taken in parts across resends instead.
	if room := sharedRoom(queue); room <= 0 || (room < len(calls) && len(calls) <= share(queue)) {
		return false
	}

	tenant := tenantOf(reporter)
	ids := make([]Call, len(calls))
	for i, c := range calls {
		ids[i] = c.id
	}
	fresh := h.mark(ctx, tenant, MarkerUsage, ids)

	var unqueued []Call
	// recorded are the calls with a usage row, queued now or by an earlier export: the ones whose content may be stored.
	recorded := calls[:0:0]
	for i, c := range calls {
		if !fresh[i] {
			sig.records.WithLabelValues(outcomeDuplicate).Inc()
			recorded = append(recorded, c)
			continue
		}
		hint := h.opts.ProviderHints[strings.ToLower(strings.TrimSpace(c.inf.Provider))]
		if len(unqueued) > 0 || sharedRoom(queue) <= 0 || !queue.TryEmit(buildEvent(snap, reporter, c, hint, h.opts.Pricer, from, now)) {
			unqueued = append(unqueued, c.id)
			continue
		}
		sig.records.WithLabelValues(outcomeRecorded).Inc()
		recorded = append(recorded, c)
	}
	h.storeContent(ctx, tenant, reporter, recorded, now)
	if len(unqueued) == 0 {
		return true
	}
	h.unmark(ctx, tenant, MarkerUsage, unqueued, "otlp receiver: calls stay marked as recorded but were not queued")
	return false
}

// storeContent queues the reported message content of calls for the payload store, once per call, when the operator's switches are on and the reporter's policy allows it. Content is secondary to the usage row: when the payload queue lacks room it is dropped and counted rather than holding up the export, and left unmarked so a later report of the same call can still store it.
func (h *Handler) storeContent(ctx context.Context, tenant string, reporter *lifecycle.Context, calls []reported, received time.Time) {
	if !h.capturesContent() {
		return
	}
	var with []reported
	for _, c := range calls {
		if !c.inf.Content.Empty() {
			with = append(with, c)
		}
	}
	if len(with) == 0 {
		return
	}
	// A policy that governs the reporter decides, over the client's choice to send content and over the key's own flag. A disabled policy stores nothing. With no policy, what the client sent is kept.
	if pol := inference.GoverningPolicy(ctx); pol != nil && (!pol.IsEnabled() || !pol.Spec.PayloadLoggingEnabled) {
		contentTotal.WithLabelValues(contentPolicy).Add(float64(len(with)))
		return
	}
	queue := h.opts.Payloads
	if sharedRoom(queue) <= 0 {
		contentTotal.WithLabelValues(contentDropped).Add(float64(len(with)))
		return
	}

	ids := make([]Call, len(with))
	for i, c := range with {
		ids[i] = c.id
	}
	fresh := h.mark(ctx, tenant, MarkerContent, ids)
	maxBytes := h.opts.PayloadLog.MaxBytes()
	var unqueued []Call
	for i, c := range with {
		if !fresh[i] {
			contentTotal.WithLabelValues(contentDuplicate).Inc()
			continue
		}
		if sharedRoom(queue) <= 0 || !queue.TryEmit(buildPayload(reporter, c, received, maxBytes)) {
			unqueued = append(unqueued, c.id)
			continue
		}
		contentTotal.WithLabelValues(contentStored).Inc()
	}
	if len(unqueued) > 0 {
		contentTotal.WithLabelValues(contentDropped).Add(float64(len(unqueued)))
		h.unmark(ctx, tenant, MarkerContent, unqueued, "otlp receiver: content stays marked as stored but was not queued")
	}
}

func (h *Handler) capturesContent() bool {
	o := h.opts
	return o.CaptureContent != nil && o.PayloadLog != nil && o.Payloads != nil && o.CaptureContent() && o.PayloadLog.Enabled()
}

// mark reports which calls have not had kind stored before. A failing store answers "none had": storing a call twice is the smaller harm than losing it.
func (h *Handler) mark(ctx context.Context, tenant string, kind MarkerKind, ids []Call) []bool {
	if h.opts.Markers != nil {
		fresh, err := h.opts.Markers.Mark(ctx, tenant, kind, ids)
		if err != nil {
			markerErrors.WithLabelValues(opMark).Inc()
			slog.Default().Warn("otlp receiver: duplicate check failed; recording without it", "kind", string(kind), "err", err)
		}
		return fresh
	}
	fresh := make([]bool, len(ids))
	for i := range fresh {
		fresh[i] = true
	}
	return fresh
}

// unmark removes the markers of calls that were marked and then not queued, so a resend stores them.
func (h *Handler) unmark(ctx context.Context, tenant string, kind MarkerKind, ids []Call, onFailure string) {
	if h.opts.Markers == nil {
		return
	}
	// Detached from the request: a client that hangs up must not leave calls marked that were never queued.
	if err := h.opts.Markers.Unmark(context.WithoutCancel(ctx), tenant, kind, ids); err != nil {
		markerErrors.WithLabelValues(opUnmark).Inc()
		slog.Default().Warn(onFailure, "calls", len(ids), "err", err)
	}
}
