package otlpreceiver

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// record queues one usage event per call not recorded before, then stores the content of the calls that carry any and reports how much of it was withheld. It reports false when the usage queue could not take them all: the client must then send the export again, and whatever was queued this time is skipped as a duplicate.
func (h *Handler) record(ctx context.Context, sig signal, snap *appcatalog.Snapshot, reporter *lifecycle.Context, governing *policy.Policy, from origin, calls []reported, now time.Time) (bool, withheld) {
	queue := h.opts.Usage
	// Refused before anything is marked when the export cannot fit. An export larger than the receiver's whole share never fits, so it is taken in parts across resends instead.
	if room := sharedRoom(queue); room <= 0 || (room < len(calls) && len(calls) <= share(queue)) {
		return false, withheld{}
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
	kept := h.storeContent(ctx, tenant, reporter, governing, recorded, now)
	if len(unqueued) == 0 {
		return true, kept
	}
	h.unmark(ctx, tenant, MarkerUsage, unqueued, "otlp receiver: calls stay marked as recorded but were not queued")
	return false, withheld{}
}

// withheld counts the calls of one export whose content was not stored by the operator's decision, which the client is told so it can stop sending it. Content lost to a busy queue or skipped as a duplicate is not counted: the client can do nothing about either.
type withheld struct {
	// captureOff counts calls whose content arrived while content capture or payload logging was off.
	captureOff int
	// byPolicy counts calls whose content the reporter's policy does not store.
	byPolicy int
	// expired counts calls that started before the payload store's retention window, retentionDays long.
	expired       int
	retentionDays int
}

// message is the warning for the client, "" when all content sent was eligible to be stored.
func (w withheld) message() string {
	n, why := w.captureOff, "this server does not capture message content"
	if w.byPolicy > 0 {
		n, why = w.byPolicy, "the policy of this credential does not allow storing message content"
	}
	advice := "It is safe to stop sending content"
	if w.expired > 0 {
		n, why = w.expired, "the calls are older than the content retention of "+strconv.Itoa(w.retentionDays)+" days"
		advice = "It is safe to leave content out of calls that old"
	}
	if n == 0 {
		return ""
	}
	return "message content of " + strconv.Itoa(n) + " model calls was not stored: " + why + ". " + advice + "; usage was recorded."
}

// storeContent queues the reported message content of calls for the payload store, once per call, when the operator's switches are on and the reporter's policy allows it. Content is secondary to the usage row: when the payload queue lacks room it is dropped and counted rather than holding up the export, and left unmarked so a later report of the same call can still store it.
func (h *Handler) storeContent(ctx context.Context, tenant string, reporter *lifecycle.Context, governing *policy.Policy, calls []reported, received time.Time) withheld {
	var with []reported
	for _, c := range calls {
		if !c.inf.Content.Empty() {
			with = append(with, c)
		}
	}
	if len(with) == 0 {
		return withheld{}
	}
	if !h.capturesContent() {
		return withheld{captureOff: len(with)}
	}
	// A policy that governs the reporter decides, over the client's choice to send content and over the key's own flag. With no policy, what the client sent is kept.
	if governing != nil && !governing.Spec.PayloadLoggingEnabled {
		contentTotal.WithLabelValues(contentPolicy).Add(float64(len(with)))
		return withheld{byPolicy: len(with)}
	}
	// The payload store deletes by age from the call's start, which may be a shorter window than the usage store's: such a call keeps its usage row and loses only its content.
	var kept withheld
	if oldest, days := oldestKept(h.opts.ContentRetention, received); !oldest.IsZero() {
		current := with[:0]
		for _, c := range with {
			if !c.inf.Start.IsZero() && c.inf.Start.Before(oldest) {
				kept.expired++
				continue
			}
			current = append(current, c)
		}
		if with = current; kept.expired > 0 {
			kept.retentionDays = days
			contentTotal.WithLabelValues(contentExpired).Add(float64(kept.expired))
		}
		if len(with) == 0 {
			return kept
		}
	}
	queue := h.opts.Payloads
	if sharedRoom(queue) <= 0 {
		contentTotal.WithLabelValues(contentDropped).Add(float64(len(with)))
		return kept
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
	return kept
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
