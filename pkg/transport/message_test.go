package transport

import (
	"context"
	"testing"
)

// NewChannel sizes both directions as asked and derives its context from the
// parent: cancelling either the parent or the channel ends it.
func TestNewChannel(t *testing.T) {
	ch := NewChannel(context.Background(), "req-1", 1, 4)
	if ch.ID != "req-1" {
		t.Fatalf("ID = %q, want req-1", ch.ID)
	}
	if cap(ch.In) != 1 || cap(ch.Out) != 4 {
		t.Fatalf("buffers = in %d / out %d, want 1 / 4", cap(ch.In), cap(ch.Out))
	}
	if ch.Ctx.Err() != nil {
		t.Fatalf("fresh channel context already done: %v", ch.Ctx.Err())
	}
	ch.Cancel()
	if ch.Ctx.Err() != context.Canceled {
		t.Fatalf("after Cancel: ctx err = %v, want context.Canceled", ch.Ctx.Err())
	}

	parent, cancelParent := context.WithCancel(context.Background())
	child := NewChannel(parent, "req-2", 0, 0)
	defer child.Cancel()
	cancelParent()
	<-child.Ctx.Done()
	if child.Ctx.Err() != context.Canceled {
		t.Fatalf("after parent cancel: ctx err = %v, want context.Canceled", child.Ctx.Err())
	}
}
