// Package otlpreceiver records model calls that clients report over OpenTelemetry instead of sending through relay. It serves the OTLP/HTTP trace export endpoint: each exported span that a registered convention recognises as a model call becomes a usage event attributed to the authenticated caller and priced from the catalog.
//
// Reported usage is self-declared, so nothing here is an enforcement point: no policy, rate limit or key pool is consulted, and every event carries the source "otlp" so readers can tell it from traffic relay carried. Spans that are not model calls are not stored. Message content and the logs and metrics signals are out of scope.
package otlpreceiver
