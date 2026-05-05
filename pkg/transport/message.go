package transport

import (
	"context"
	"time"
)

// Message is the canonical packet that flows through Relay. It is
// transport-agnostic and payload-type-agnostic. There are NO
// transport flags (no Stream, no protocol field) and NO payload-shape
// flags (no Model, no provider-specific field). Anything OpenAI-shape
// or Ollama-shape is extracted by handlers from Body into locals.
type Message struct {
	ID          string
	ParentID    string
	Body        []byte
	Headers     map[string]string
	Labels      map[string]string
	Attribution map[string]string
	ReceivedAt  time.Time
}

// Channel is a transport-agnostic processing context. One Channel
// services one request (sync HTTP) or many messages (future batch).
//
// Channel direction: In and Out are sized from the pipeline's
// perspective. The pipeline READS from In and WRITES to Out. The
// transport adapter writes inbound payloads to In and reads response
// payloads from Out.
type Channel struct {
	ID     string
	In     chan *Message
	Out    chan *Message
	Ctx    context.Context
	Cancel context.CancelFunc
}

// NewChannel constructs a Channel with caller-controlled buffer sizes.
// inBuf is typically 1 (one inbound message per sync request); outBuf
// should be large enough to absorb streaming chunks without blocking
// the upstream reader (e.g., 64).
//
// The Channel takes ownership of its Ctx; callers should defer Cancel
// to release resources.
func NewChannel(parent context.Context, id string, inBuf, outBuf int) *Channel {
	ctx, cancel := context.WithCancel(parent)
	return &Channel{
		ID:     id,
		In:     make(chan *Message, inBuf),
		Out:    make(chan *Message, outBuf),
		Ctx:    ctx,
		Cancel: cancel,
	}
}
