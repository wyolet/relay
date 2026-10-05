// Package otlp is the wire side of an OpenTelemetry trace receiver over OTLP/HTTP: it reads and decodes export requests (protobuf and JSON), exposes their spans in a neutral shape, and encodes the export response. A SpanMapper per telemetry convention turns the spans that describe model calls into Inference records.
//
// Authentication, attribution, pricing and storage belong to the caller. The logs and metrics signals and the gRPC transport are out of scope.
package otlp
