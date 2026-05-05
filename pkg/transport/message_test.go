package transport

import (
	"context"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ch := NewChannel(context.Background(), "test-1", 1, 4)
	defer ch.Cancel()

	go func() {
		ch.In <- &Message{ID: "req-1", Body: []byte("hello")}
		close(ch.In)
	}()

	go func() {
		msg, ok := <-ch.In
		if !ok || msg == nil {
			return
		}
		for i := range 3 {
			m := &Message{ID: msg.ID, Headers: map[string]string{}}
			if i == 2 {
				m.Headers["X-Relay-Final"] = "true"
			}
			ch.Out <- m
		}
		close(ch.Out)
	}()

	var msgs []*Message
	for m := range ch.Out {
		msgs = append(msgs, m)
	}

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[2].Headers["X-Relay-Final"] != "true" {
		t.Fatalf("third message missing X-Relay-Final marker")
	}

	_, ok := <-ch.Out
	if ok {
		t.Fatal("expected closed channel to return ok=false")
	}
}

func TestCancelStopsConsumer(t *testing.T) {
	ch := NewChannel(context.Background(), "test-2", 1, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		msg, ok := <-ch.In
		if !ok || msg == nil {
			return
		}
		for {
			select {
			case ch.Out <- &Message{ID: msg.ID}:
			case <-ch.Ctx.Done():
				return
			}
		}
	}()

	ch.In <- &Message{ID: "req-2"}
	ch.Cancel()
	<-done

	if ch.Ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", ch.Ctx.Err())
	}
}

func TestParentContextCancellation(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ch := NewChannel(parent, "test-3", 1, 1)
	defer ch.Cancel()

	parentCancel()

	<-ch.Ctx.Done()
	if ch.Ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", ch.Ctx.Err())
	}
}

func TestZeroValues(t *testing.T) {
	ch := NewChannel(context.Background(), "test-4", 0, 0)
	defer ch.Cancel()

	m := &Message{}

	go func() { ch.In <- m }()
	got := <-ch.In
	if got == nil {
		t.Fatal("expected message, got nil")
	}

	go func() { ch.Out <- m }()
	got = <-ch.Out
	if got == nil {
		t.Fatal("expected message, got nil")
	}
}

func TestAttributionRoundTrip(t *testing.T) {
	ch := NewChannel(context.Background(), "test-attr", 1, 1)
	defer ch.Cancel()

	attr := map[string]string{"env": "prod", "team": "relay"}
	in := &Message{ID: "req-attr", Attribution: attr}

	go func() {
		ch.In <- in
		close(ch.In)
	}()

	got := <-ch.In
	if got.Attribution == nil {
		t.Fatal("Attribution is nil after round-trip")
	}
	if got.Attribution["env"] != "prod" || got.Attribution["team"] != "relay" {
		t.Errorf("Attribution mismatch: %v", got.Attribution)
	}
}
