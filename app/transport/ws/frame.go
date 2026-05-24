// Package ws is a customer-facing WebSocket transport for the inference
// data plane. A single long-lived connection multiplexes many inference
// requests, each correlated by a caller-chosen id, so chatty clients
// (IDE / agent loops) pay the TLS + auth handshake once instead of per
// turn.
//
// The transport is a thin shell: it owns the frame protocol and the
// connection pumps, then drives an injected http.HandlerFunc per frame
// via a synthetic http.ResponseWriter (writer.go). It imports neither
// app/httpapi/inference nor app/pipeline — the handler closure wires
// those in. This keeps dispatch, routing, and the pipeline untouched;
// the same code path serves HTTP and WS.
//
// Out of scope here: which wire shape a frame carries (the handler
// decides), authentication (done by middleware on the upgrade request),
// and rate limiting (the pipeline the handler invokes owns it).
package ws

import "encoding/json"

// clientFrame is one inbound request multiplexed on the connection.
// Payload is the raw inference request body (e.g. a pkg/relay/v1
// canonical request); the transport forwards it verbatim as the
// synthetic request body without inspecting it.
type clientFrame struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// Outbound event kinds. Every request emits exactly one start, zero or
// more chunks, then exactly one end — in order, though frames for
// different ids interleave on the wire.
const (
	eventStart = "start"
	eventChunk = "chunk"
	eventEnd   = "end"
)

// serverFrame is one outbound event tagged with the request id it
// belongs to. Data is a raw byte-stream fragment of the response body
// (an SSE event for streaming, the whole JSON body for buffered
// responses); clients concatenate Data across chunk frames for a given
// id and parse the result themselves. Framing is a byte stream, not an
// event boundary.
type serverFrame struct {
	ID      string            `json:"id"`
	Event   string            `json:"event"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Data    string            `json:"data,omitempty"`
}
