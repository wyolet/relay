// Package otlpreceiver records model calls that clients report over OpenTelemetry instead of sending through relay. It serves the OTLP/HTTP trace export endpoint: each exported span that a registered convention recognises as a model call becomes a usage event attributed to the authenticated caller and priced from the catalog.
//
// Reported usage is self-declared, so nothing here is an enforcement point: no policy or key pool is consulted, and every event carries the source "otlp" so readers can tell it from traffic relay carried. The one limit applied is the system rate limit on export requests, which protects the receiver itself. Spans that are not model calls are not stored. Message content and the logs and metrics signals are out of scope.
//
// Exporters retry, so a call is identified by its trace id and span id and recorded once: an export that cannot be queued whole is refused and sent again, and the calls already recorded are skipped.
//
// Expected kv ops per export: 1 RunScript for the rate limit reservation, and 1 batched round trip marking the export's calls (one script call per distinct trace id, pipelined when the store batches). A second batched round trip removes markers only when the usage queue filled while the export was being queued.
package otlpreceiver
