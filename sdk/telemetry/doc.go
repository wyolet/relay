// Package telemetry reports model calls made directly to a provider to an OTLP/HTTP endpoint, relay's receiver or any other backend, in the OpenTelemetry GenAI semantic conventions.
//
// One Call becomes one CLIENT span carrying the request, response and token usage, and, only when message content is sent, one gen_ai.client.inference.operation.details log record with the same trace id and span id that carries the same attributes plus the content. Requests are OTLP/HTTP JSON, gzip-compressed, written by hand so the package depends on nothing beyond the standard library and the SDK's canonical types.
//
// Recording never blocks and never fails a model call: calls wait in a bounded queue, a full queue drops and counts, and one goroutine exports them in batches with bounded retries. Whether content is sent follows the receiving relay's answer in the X-WR-Content-Capture response header when it gives one, the client's opt-in otherwise.
//
// Out of scope: metrics, the per-message events of earlier conventions, OTLP over protobuf or gRPC, and instrumenting third-party HTTP clients.
package telemetry
