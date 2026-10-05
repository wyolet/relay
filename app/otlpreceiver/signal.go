package otlpreceiver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/otlp"
)

// signal is what differs between the export endpoints: how a body becomes model calls, how a call is identified, and what its records are called.
type signal struct {
	wire otlp.Signal
	// noun names the signal's records in messages to the client.
	noun string
	// records counts the signal's records by outcome.
	records *prometheus.CounterVec
	// responseIdentity lets a call that carries no span ids be identified by the provider's response id. A span always has ids of its own; an event emitted outside a span has none.
	responseIdentity bool
	// decode returns the model calls an export body reports and how many records it held in all.
	decode func(h *Handler, mediaType string, body []byte) (calls []otlp.Inference, total int, err error)
}

var traces = signal{
	wire:    otlp.SignalTraces,
	noun:    "spans",
	records: spansTotal,
	decode: func(h *Handler, mediaType string, body []byte) ([]otlp.Inference, int, error) {
		spans, err := otlp.DecodeTraces(mediaType, body)
		if err != nil {
			return nil, 0, err
		}
		var calls []otlp.Inference
		for _, s := range spans {
			for _, m := range h.opts.SpanMappers {
				if inf, ok := m.MapSpan(s); ok {
					calls = append(calls, inf)
					break
				}
			}
		}
		return calls, len(spans), nil
	},
}

var logs = signal{
	wire:             otlp.SignalLogs,
	noun:             "log records",
	records:          logRecordsTotal,
	responseIdentity: true,
	decode: func(h *Handler, mediaType string, body []byte) ([]otlp.Inference, int, error) {
		records, err := otlp.DecodeLogs(mediaType, body)
		if err != nil {
			return nil, 0, err
		}
		var calls []otlp.Inference
		for _, r := range records {
			for _, m := range h.opts.LogMappers {
				if inf, ok := m.MapLog(r); ok {
					calls = append(calls, inf)
					break
				}
			}
		}
		return calls, len(records), nil
	},
}

func (s signal) result(result string) prometheus.Counter {
	return exportsTotal.WithLabelValues(string(s.wire), result)
}

// responseSpan stands where the span id does in the identity of a call known only by its response id. It is not hex, so such an identity never equals a span's.
const responseSpan = "response"

// reported is one model call of an export with the identity it is recorded under.
type reported struct {
	id  Call
	inf otlp.Inference
}

// identify returns the identity a call is recorded under: the ids of its span, which the span and an event of the same call share, else (when byResponse) a digest of the provider's response id. The digest keeps arbitrary provider text out of kv keys and request ids.
func identify(inf otlp.Inference, byResponse bool) (Call, bool) {
	if inf.TraceID != "" && inf.SpanID != "" {
		return Call{TraceID: inf.TraceID, SpanID: inf.SpanID}, true
	}
	if byResponse && inf.ResponseID != "" {
		sum := sha256.Sum256([]byte(inf.ResponseID))
		return Call{TraceID: hex.EncodeToString(sum[:16]), SpanID: responseSpan}, true
	}
	return Call{}, false
}

// refusals counts the model calls of one export that are not recorded, by reason.
type refusals struct {
	noIdentity int64
	future     int64
	// repeated counts calls that appear more than once in the export. They are duplicates, not rejections.
	repeated int64
}

func (r refusals) rejected() int64 { return r.noIdentity + r.future }

func (r refusals) message(sig signal) string {
	var parts []string
	if r.noIdentity > 0 {
		missing := " without a trace id or span id"
		if sig.responseIdentity {
			missing = " with neither span ids nor a response id"
		}
		parts = append(parts, strconv.FormatInt(r.noIdentity, 10)+missing)
	}
	if r.future > 0 {
		parts = append(parts, fmt.Sprintf("%d starting more than %d minutes in the future", r.future, int(maxClockSkew/time.Minute)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "model-call " + sig.noun + " rejected: " + strings.Join(parts, "; ")
}

// admit returns the calls that may be recorded, each once, and counts the rest.
func admit(calls []otlp.Inference, byResponse bool, now time.Time) ([]reported, refusals) {
	var refused refusals
	latestStart := now.Add(maxClockSkew)
	inExport := make(map[Call]struct{}, len(calls))
	accepted := make([]reported, 0, len(calls))
	for _, inf := range calls {
		id, ok := identify(inf, byResponse)
		switch {
		// Without an identity a resent record could not be told from a new one.
		case !ok:
			refused.noIdentity++
			continue
		case inf.Start.After(latestStart):
			refused.future++
			continue
		}
		if _, seen := inExport[id]; seen {
			refused.repeated++
			continue
		}
		inExport[id] = struct{}{}
		accepted = append(accepted, reported{id: id, inf: inf})
	}
	return accepted, refused
}
