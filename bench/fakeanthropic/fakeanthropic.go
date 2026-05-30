// Package fakeanthropic is a test stub that replays captured Claude Code session
// responses as a minimal Anthropic Messages API server. It supports both non-streaming
// (application/json) and streaming (text/event-stream SSE) responses.
//
// Streaming synthesis: each content block is emitted as a complete chunk rather than
// character-by-character. This is sufficient for end-to-end pipeline tests and avoids
// introducing artificial latency or complex chunking logic.
package fakeanthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// message is the subset of the Anthropic Messages response we care about.
type message struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        json.RawMessage   `json:"usage"`
}

type sessionLine struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

// Server is a fake Anthropic upstream. Create with New, register its handler.
type Server struct {
	responses []json.RawMessage
	idx       atomic.Uint64

	// LatencyMin/Max inject a uniform-random sleep before each response.
	// Zero values disable. Applied to both streaming and non-streaming.
	LatencyMin time.Duration
	LatencyMax time.Duration
}

// LoadSession reads a Claude Code JSONL session file and extracts assistant turns.
func LoadSession(path string) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		var sl sessionLine
		if err := json.Unmarshal(sc.Bytes(), &sl); err != nil || sl.Type != "assistant" {
			continue
		}
		if len(sl.Message) == 0 {
			continue
		}
		out = append(out, sl.Message)
	}
	return out, sc.Err()
}

// New creates a Server replaying the given canned responses.
func New(responses []json.RawMessage) *Server {
	return &Server{responses: responses}
}

// Handler returns an http.Handler for POST /v1/messages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	return mux
}

func (s *Server) injectLatency(ctx context.Context) {
	if s.LatencyMax <= 0 {
		return
	}
	min, max := s.LatencyMin, s.LatencyMax
	if min < 0 {
		min = 0
	}
	if max < min {
		max = min
	}
	d := min
	if span := max - min; span > 0 {
		d += time.Duration(rand.Int64N(int64(span)))
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (s *Server) next() json.RawMessage {
	if len(s.responses) == 0 {
		return json.RawMessage(`{"id":"msg_empty","type":"message","role":"assistant","model":"claude-opus-4-7","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	}
	i := s.idx.Add(1) - 1
	return s.responses[i%uint64(len(s.responses))]
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") == "" {
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"missing x-api-key"}}`, http.StatusUnauthorized)
		return
	}

	s.injectLatency(r.Context())

	raw := s.next()

	// Peek at stream flag.
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.Stream {
		s.streamResponse(w, raw)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) streamResponse(w http.ResponseWriter, raw json.RawMessage) {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	fl, canFlush := w.(http.Flusher)
	emit := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if canFlush {
			fl.Flush()
		}
	}

	// message_start: envelope without content
	envelope := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msg.ID,
			"type":          "message",
			"role":          msg.Role,
			"model":         msg.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         json.RawMessage(`{"input_tokens":0,"output_tokens":0}`),
		},
	}
	emitJSON(emit, "message_start", envelope)

	// Occasional ping (1 in 3 chance before content).
	if rand.IntN(3) == 0 {
		emit("ping", `{"type":"ping"}`)
	}

	for i, blockRaw := range msg.Content {
		var block map[string]any
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		btype, _ := block["type"].(string)

		// content_block_start
		startPayload := map[string]any{
			"type":  "content_block_start",
			"index": i,
		}
		switch btype {
		case "text":
			startPayload["content_block"] = map[string]any{"type": "text", "text": ""}
		case "tool_use":
			startPayload["content_block"] = map[string]any{
				"type":  "tool_use",
				"id":    block["id"],
				"name":  block["name"],
				"input": map[string]any{},
			}
		default:
			startPayload["content_block"] = map[string]any{"type": btype}
		}
		emitJSON(emit, "content_block_start", startPayload)

		// content_block_delta
		switch btype {
		case "text":
			text, _ := block["text"].(string)
			deltaPayload := map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}
			emitJSON(emit, "content_block_delta", deltaPayload)
		case "tool_use":
			inputBytes, _ := json.Marshal(block["input"])
			deltaPayload := map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(inputBytes)},
			}
			emitJSON(emit, "content_block_delta", deltaPayload)
		}

		// content_block_stop
		emitJSON(emit, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}

	// message_delta
	emitJSON(emit, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": msg.StopReason, "stop_sequence": nil},
		"usage": msg.Usage,
	})

	emit("message_stop", `{"type":"message_stop"}`)
}

func emitJSON(emit func(string, string), event string, v any) {
	b, _ := json.Marshal(v)
	emit(event, string(b))
}
