package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Event is one canonical stream event: its name (a v1.Event* constant) and the
// raw JSON payload. Decode Data into the matching v1 event struct as needed.
type Event struct {
	Type string
	Data []byte
}

// Stream iterates canonical events. For a relay target the upstream stream is
// already canonical (toCanon is nil, frames pass through); for a vendor target
// toCanon converts each vendor SSE frame to canonical.
type Stream struct {
	body    io.ReadCloser
	sc      *bufio.Scanner
	toCanon func([]byte) ([]byte, error)
	pending [][]byte // canonical frames produced from one upstream frame, not yet returned

	binding catalog.Binding
	priced  bool
	usage   usage.Tokens

	// Timing, tracked as the caller drains via Recv. Offsets from start
	// (the GenerateStream call), surfaced through Timing() in the exact
	// usage.{Upstream,Reasoning}Timing shape relay ships — so standalone-
	// client data never drifts from through-relay data.
	start          time.Time
	firstByte      time.Duration // first event received (TTFT)
	firstByteSeen  bool
	end            time.Duration // stream closed (io.EOF)
	reasoningStart time.Duration
	reasoningEnd   time.Duration
	reasoningSeen  bool
}

// Recv returns the next canonical event, or io.EOF at end.
func (s *Stream) Recv() (*Event, error) {
	for {
		if len(s.pending) > 0 {
			frame := s.pending[0]
			s.pending = s.pending[1:]
			if event, data, ok := v1.ParseSSEChunk(frame); ok {
				now := time.Since(s.start)
				if !s.firstByteSeen {
					s.firstByte = now
					s.firstByteSeen = true
				}
				if v1.IsReasoningEvent(event, data) {
					if !s.reasoningSeen {
						s.reasoningStart = now
						s.reasoningSeen = true
					}
					s.reasoningEnd = now
				}
				if event == v1.EventGenerationCompleted {
					var ev v1.GenerationCompletedEvent
					if json.Unmarshal(data, &ev) == nil && len(ev.Usage) > 0 {
						s.usage = ev.Usage
					}
				}
				return &Event{Type: event, Data: append([]byte(nil), data...)}, nil
			}
			continue
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return nil, err
			}
			s.end = time.Since(s.start)
			return nil, io.EOF
		}
		raw := append(append([]byte(nil), s.sc.Bytes()...), '\n', '\n')
		if s.toCanon == nil {
			s.pending = [][]byte{raw}
			continue
		}
		out, err := s.toCanon(raw)
		if err != nil {
			return nil, err
		}
		s.pending = splitFrames(out)
	}
}

// Close releases the underlying response body.
func (s *Stream) Close() error { return s.body.Close() }

// Cost returns total cost from the target's pricing rate sheet after the
// stream's terminal usage event has been received. ok is false for relay
// targets, unpriced hosts, and when usage is not yet available.
func (s *Stream) Cost() (float64, bool) {
	if !s.priced || len(s.usage) == 0 {
		return 0, false
	}
	return s.binding.Cost(s.usage)
}

// StreamTiming is the client-side timing of one streamed generation, in the
// exact shape relay ships server-side (usage.UpstreamTiming +
// usage.ReasoningTiming, microseconds from the GenerateStream call). A
// consumer reads identical fields whether it called relay or a provider
// directly — no drift, no branching on the path.
//
// The client has no separate relay→upstream handoff, so Upstream.Start is
// collapsed onto ResponseStart (both = TTFT); when relay is in the path the
// relay server records its own earlier Start in its own event.
type StreamTiming struct {
	Upstream  usage.UpstreamTiming   // Start == ResponseStart == TTFT; ResponseEnd == close
	Reasoning *usage.ReasoningTiming // nil when the stream carried no reasoning
}

// Timing reports the stream timing observed so far. Call it after draining
// the stream for the full picture; mid-stream it reflects events up to the
// last Recv (ResponseEnd is zero until io.EOF).
func (s *Stream) Timing() StreamTiming {
	t := StreamTiming{Upstream: usage.UpstreamTiming{
		Start:         s.firstByte.Microseconds(),
		ResponseStart: s.firstByte.Microseconds(),
		ResponseEnd:   s.end.Microseconds(),
	}}
	if s.reasoningSeen {
		t.Reasoning = &usage.ReasoningTiming{
			Start: s.reasoningStart.Microseconds(),
			End:   s.reasoningEnd.Microseconds(),
		}
	}
	return t
}

// splitSSEFrames is a bufio.SplitFunc yielding one SSE frame per token.
func splitSSEFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf >= 0 && (crlf < 0 || lf <= crlf):
		return lf + 2, data[:lf], nil
	case crlf >= 0:
		return crlf + 4, data[:crlf], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// splitFrames splits concatenated SSE bytes into individual frames.
func splitFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			if len(bytes.TrimSpace(b)) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		if len(bytes.TrimSpace(b[:i])) > 0 {
			frames = append(frames, b[:i+2])
		}
		b = b[i+2:]
	}
	return frames
}
