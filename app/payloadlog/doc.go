// Package payloadlog is the second lifecycle observer (after app/usagelog):
// it captures the full request + response bodies of opted-in requests for
// audit, debug, and replay, and pushes them to a Sink (file today, S3
// next) off the hot path.
//
// It mirrors usagelog's shape exactly — a produce → attach → store flow on
// the shared lifecycle Registry:
//
//   - PayloadHook (buffered path): builds a Record from lc.RequestBody +
//     PostFlightEvent.ResponseBody.
//   - StreamPayloadFactory (streamed path): accumulates upstream SSE frames
//     and builds the Record at end-of-stream. Returns a no-op observer when
//     capture is disabled, so non-opted-in streams never buffer.
//   - SinkCollector (janitor): reads the attached Record and emits it onto
//     the bounded, drop-on-full Emitter → Sink.
//
// Capture is OFF by default and gated per request via lc.PayloadLog, set at
// the inference entry from the routing Plan (Policy or RelayKey opt-in).
// Bodies are stored raw; the only transform is a configurable size cap that
// truncates oversized bodies and flags the Record. Operator-internal: there
// is no read endpoint — payloads are an offline artifact, joinable to usage
// events by request_id.
package payloadlog
