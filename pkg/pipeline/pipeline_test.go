package pipeline

import (
	"context"
	"testing"

	"github.com/wyolet/relay/pkg/transport"
)

func newTestChannel(ctx context.Context) *transport.Channel {
	return transport.NewChannel(ctx, "test", 1, 64)
}

func TestRun_Passthrough(t *testing.T) {
	ctx := context.Background()
	ch := newTestChannel(ctx)

	inMsg := &transport.Message{ID: "1", Body: []byte("hello")}
	ch.In <- inMsg

	outbound := func(ctx context.Context, body []byte, out chan<- *transport.Message) error {
		defer close(out)
		out <- &transport.Message{Body: []byte("chunk1")}
		out <- &transport.Message{Body: []byte("chunk2")}
		out <- &transport.Message{Body: []byte("chunk3"), Headers: map[string]string{"X-Relay-Final": "true"}}
		return nil
	}

	err := Run(ctx, ch, outbound)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var msgs []*transport.Message
	for m := range ch.Out {
		msgs = append(msgs, m)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
}

func TestRun_NoInboundMessage(t *testing.T) {
	ctx := context.Background()
	ch := newTestChannel(ctx)
	close(ch.In)

	err := Run(ctx, ch, nil)
	if err != ErrNoInboundMessage {
		t.Fatalf("expected ErrNoInboundMessage, got %v", err)
	}
}

func TestRun_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := newTestChannel(ctx)
	cancel()

	err := Run(ctx, ch, nil)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
