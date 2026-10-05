// Package otlp is the wire side of an OpenTelemetry receiver over OTLP/HTTP for the traces and logs signals: it reads and decodes export requests (protobuf and JSON), exposes their spans and log records in a neutral shape, and encodes the export response. A SpanMapper or LogMapper per telemetry convention turns the records that describe model calls into Inference records.
//
// Authentication, attribution, pricing and storage belong to the caller. The metrics signal and the gRPC transport are out of scope.
package otlp
