package coord

import (
	"context"
	"sync/atomic"
	"time"
)

// Notifier is pub/sub with a bounded outbound queue: Publish never blocks, a
// full queue or a failed send drops the message and counts it. Transitions
// publish here; a Watcher's watermark bounds what a lost message costs.
type Notifier struct {
	s       Store
	q       chan message
	timeout time.Duration
	onDrop  func()
	active  atomic.Bool // a sender goroutine is draining q
}

type message struct {
	topic   string
	payload []byte
}

// NewNotifier returns a notifier holding at most queue unsent messages. timeout bounds each send (0 means one second), so a hung store drops messages instead of stalling the queue. onDrop, when set, is called once per dropped message.
func NewNotifier(s Store, queue int, timeout time.Duration, onDrop func()) *Notifier {
	if timeout <= 0 {
		timeout = time.Second
	}
	return &Notifier{s: s, q: make(chan message, max(queue, 1)), timeout: timeout, onDrop: onDrop}
}

// Publish queues payload for topic; never blocks.
func (n *Notifier) Publish(topic string, payload []byte) {
	select {
	case n.q <- message{topic, payload}:
	default:
		n.drop()
		return
	}
	if n.active.CompareAndSwap(false, true) {
		go n.send()
	}
}

// send drains the queue and exits when it is empty, so an idle notifier holds
// no goroutine and needs no Close.
func (n *Notifier) send() {
	for {
		select {
		case m := <-n.q:
			ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
			err := n.s.Publish(ctx, m.topic, m.payload)
			cancel()
			if err != nil {
				n.drop()
			}
		default:
			n.active.Store(false)
			// A Publish between the empty check and the store above saw
			// active and did not start a sender; pick its message up.
			if len(n.q) == 0 || !n.active.CompareAndSwap(false, true) {
				return
			}
		}
	}
}

func (n *Notifier) drop() {
	if n.onDrop != nil {
		n.onDrop()
	}
}

// Subscribe delivers topic's payloads to fn until ctx ends.
func (n *Notifier) Subscribe(ctx context.Context, topic string, fn func([]byte)) error {
	stop, err := n.s.Subscribe(ctx, topic, fn)
	if err != nil {
		return err
	}
	context.AfterFunc(ctx, stop)
	return nil
}
