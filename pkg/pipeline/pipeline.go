package pipeline

import (
	"context"
	"errors"

	"github.com/wyolet/relay/pkg/transport"
)

// Outbound sends a request body to a destination and emits the response
// as a stream of *Messages on out. The Outbound is responsible for
// closing out before returning. The first emitted Message must carry
// any HTTP-status / Content-Type semantics in its Headers (see chat.go
// for the contract); subsequent Messages contribute body bytes only;
// the final Message marks Headers["X-Relay-Final"] = "true".
type Outbound func(ctx context.Context, body []byte, out chan<- *transport.Message) error

var ErrNoInboundMessage = errors.New("pipeline: no inbound message on Channel.In")

// Run reads one inbound *Message from ch.In, calls outbound with its
// body and ch.Out, and returns the outbound's error (if any).
//
// Run is intentionally minimal in M1. M2 will resolve a Pool here;
// M3 will gate RateLimits here. Today it is a passthrough.
//
// Run honors ch.Ctx for cancellation and returns ctx.Err() if the
// caller cancels before the inbound message is read.
func Run(ctx context.Context, ch *transport.Channel, outbound Outbound) error {
	select {
	case msg, ok := <-ch.In:
		if !ok {
			return ErrNoInboundMessage
		}
		return outbound(ctx, msg.Body, ch.Out)
	case <-ctx.Done():
		return ctx.Err()
	}
}
