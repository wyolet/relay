package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/metrics"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const (
	directionToCanonical   = "to_canonical"
	directionFromCanonical = "from_canonical"
)

// streamShapes names the two translators streamCanonical chains, labelling the frames either one fails to convert.
type streamShapes struct {
	upstream adapters.Name
	inbound  adapters.Name
}

// streamDrops tracks the frames one stream skipped because a translator could not convert them. Bytes may already have reached the caller and failover is pre-first-byte only, so a bad frame is skipped and made visible instead of failing the stream.
type streamDrops struct {
	ctx    context.Context
	shapes streamShapes
	count  int
}

func (s *streamDrops) record(direction string, err error) {
	s.count++
	adapter := s.shapes.upstream
	if direction == directionFromCanonical {
		adapter = s.shapes.inbound
	}
	reason := streamDropReason(err)
	metrics.StreamEventDropped(string(adapter), direction, reason)
	// The first drop is logged with its cause; the rest are counted, so a stream of unparseable frames cannot flood the log. The error names the event and decode failure, never the payload.
	if s.count == 1 {
		slog.WarnContext(s.ctx, "inference: stream event dropped",
			"adapter", string(adapter), "direction", direction, "reason", reason, "err", err)
	}
}

func (s *streamDrops) logTotal() {
	if s.count > 1 {
		slog.WarnContext(s.ctx, "inference: stream events dropped", "count", s.count,
			"upstream", string(s.shapes.upstream), "inbound", string(s.shapes.inbound))
	}
}

func streamDropReason(err error) string {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		return "syntax"
	case errors.As(err, &typeErr):
		return "type"
	default:
		return "invalid"
	}
}

// endsWithTerminalEvent reports whether the last canonical frame in canon is generation.completed or error.
func endsWithTerminalEvent(canon []byte) bool {
	canon = bytes.TrimRight(canon, "\n")
	if i := bytes.LastIndex(canon, []byte("\n\n")); i >= 0 {
		canon = canon[i+2:]
	}
	event, _, _ := v1.ParseSSEChunk(canon)
	return event == v1.EventGenerationCompleted || event == v1.EventError
}

// incompleteStreamFrame is the canonical error event that ends a stream whose terminal event was among the dropped frames, so the caller cannot read the truncated output as a complete response.
func incompleteStreamFrame() []byte {
	data, _ := json.Marshal(v1.ErrorEvent{
		Code:    "translate_stream",
		Message: "relay could not translate part of the upstream stream; the response is incomplete",
	})
	return v1.SSEFrame{Event: v1.EventError, Data: data}.Bytes()
}
