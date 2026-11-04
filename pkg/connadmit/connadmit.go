// Package connadmit admits connections to an HTTP listener: up to a cap they are served, past it they wait in a bounded FIFO for a slot for at most a fixed time, and the rest are answered with a 503 and closed. It accepts from the kernel continuously, so a connection flood never sits unanswered in the accept backlog, where SYN drops turn into silent client retransmits.
//
// Scope: the listener wrapper, the connection it hands to Serve, and an http.Server.ConnState hook that tracks idle keep-alive connections so one can be closed for a queued connection. Out of scope: request-level admission (in-flight caps and load shedding live above it), TLS, and shaping the refusal per API: the conn layer has not routed the request, so every refusal carries the one response it was given.
package connadmit

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// minIdleAge is how long a keep-alive connection must sit idle before it is closed for a queued one, so a client between two requests on it doesn't lose the second: SDKs don't retry a POST lost on a reused connection.
const minIdleAge = 500 * time.Millisecond

// Config sets the listener's limits and its refusal.
type Config struct {
	// MaxConns is the most connections handed to Serve at once; must be positive.
	MaxConns int
	// QueueSize is the most connections waiting for a slot; 0 refuses every connection past MaxConns.
	QueueSize int
	// QueueTimeout is how long a connection waits for a slot before it is refused.
	QueueTimeout time.Duration
	// Body is the JSON body of the 503 refusal.
	Body []byte
	// RetryAfter is the refusal's Retry-After value.
	RetryAfter string
	// Header holds extra headers of the refusal.
	Header http.Header
}

// Listener wraps a net.Listener with connection admission. Accept returns *Conn values.
type Listener struct {
	inner    net.Listener
	maxConns int
	wait     time.Duration
	refusal  []byte
	refusers chan struct{}

	notify  chan struct{}
	stopped chan struct{}
	// acceptErr is the inner listener's terminal Accept error, written before stopped closes.
	acceptErr error

	mu       sync.Mutex
	closed   bool
	held     int // slots held: conns handed to Serve or waiting in pending
	pending  []*Conn
	queue    []queued // ring buffer of QueueSize
	qHead    int
	qLen     int
	timer    *time.Timer
	idleHead *Conn // oldest idle conn; the list is ordered by idleSince
	idleTail *Conn
	idleLen  int
	shown    gaugeValues
}

type queued struct {
	conn     net.Conn
	deadline time.Time
}

// New starts accepting from inner. The caller passes the Listener to http.Server.Serve and sets the server's ConnState to l.ConnState.
func New(inner net.Listener, cfg Config) *Listener {
	if cfg.MaxConns <= 0 {
		panic("connadmit: MaxConns must be positive")
	}
	l := &Listener{
		inner:    inner,
		maxConns: cfg.MaxConns,
		wait:     cfg.QueueTimeout,
		refusal:  buildRefusal(cfg),
		refusers: make(chan struct{}, maxRefusers),
		notify:   make(chan struct{}, 1),
		stopped:  make(chan struct{}),
		queue:    make([]queued, max(cfg.QueueSize, 0)),
	}
	l.timer = time.AfterFunc(time.Hour, l.expireQueued)
	l.timer.Stop()
	go l.acceptLoop()
	return l
}

func buildRefusal(cfg Config) []byte {
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: ")
	b.WriteString(strconv.Itoa(len(cfg.Body)))
	b.WriteString("\r\n")
	if cfg.RetryAfter != "" {
		b.WriteString("Retry-After: " + cfg.RetryAfter + "\r\n")
	}
	b.WriteString("Connection: close\r\n")
	_ = cfg.Header.Write(&b)
	b.WriteString("\r\n")
	b.Write(cfg.Body)
	return b.Bytes()
}

// Addr returns the inner listener's address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Accept returns the next connection holding a slot. After Close, or once the inner listener fails, it returns the inner listener's error.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		l.mu.Lock()
		if !l.closed && len(l.pending) > 0 {
			c := l.pending[0]
			l.pending[0] = nil
			l.pending = l.pending[1:]
			if len(l.pending) > 0 {
				l.signal()
			}
			l.mu.Unlock()
			return c, nil
		}
		closed := l.closed
		l.mu.Unlock()
		if closed {
			<-l.stopped
			return nil, l.acceptErr
		}
		select {
		case <-l.notify:
		case <-l.stopped:
			return nil, l.acceptErr
		}
	}
}

// Close stops accepting and closes the queued connections and those not yet handed to Serve; served ones stay open for the server to close.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return l.inner.Close()
	}
	l.closed = true
	l.timer.Stop()
	var waiting []net.Conn
	for l.qLen > 0 {
		waiting = append(waiting, l.popQueued().conn)
	}
	pending := l.pending
	l.pending = nil
	l.publish()
	l.mu.Unlock()

	err := l.inner.Close()
	for _, c := range waiting {
		_ = c.Close()
	}
	for _, c := range pending {
		_ = c.Close()
	}
	return err
}

func (l *Listener) acceptLoop() {
	var backoff time.Duration
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.acceptErr = err
				close(l.stopped)
				return
			}
			// Out of file descriptors and the like: back off as net/http does rather than spin.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		l.admit(raw)
	}
}

func (l *Listener) admit(raw net.Conn) {
	now := time.Now()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = raw.Close()
		return
	}
	if l.held < l.maxConns {
		l.held++
		l.handOut(raw)
		l.publish()
		l.mu.Unlock()
		return
	}
	if l.qLen == len(l.queue) {
		l.mu.Unlock()
		l.refuse(raw, refusedQueueFull)
		return
	}
	l.queue[(l.qHead+l.qLen)%len(l.queue)] = queued{conn: raw, deadline: now.Add(l.wait)}
	l.qLen++
	if l.qLen == 1 {
		l.timer.Reset(l.wait)
	}
	victim := l.idleHead
	if victim != nil && now.Sub(victim.idleSince) >= minIdleAge {
		l.unlinkIdle(victim)
	} else {
		victim = nil
	}
	l.publish()
	l.mu.Unlock()
	if victim != nil {
		idleClosed.Inc()
		_ = victim.Close()
	}
}

// handOut queues raw, which already holds a slot, for Accept. Called with mu held; never blocks.
func (l *Listener) handOut(raw net.Conn) {
	l.pending = append(l.pending, &Conn{Conn: raw, l: l})
	l.signal()
}

func (l *Listener) signal() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// release frees c's slot, or passes it to the oldest queued connection still within its wait.
func (l *Listener) release(c *Conn) {
	now := time.Now()
	l.mu.Lock()
	c.released = true
	l.unlinkIdle(c)
	var expired []net.Conn
	handed := false
	popped := l.qLen > 0
	for l.qLen > 0 {
		q := l.popQueued()
		if !q.deadline.After(now) {
			expired = append(expired, q.conn)
			continue
		}
		l.handOut(q.conn)
		handed = true
		break
	}
	if !handed {
		l.held--
	}
	if popped {
		l.armTimer(now)
	}
	l.publish()
	l.mu.Unlock()
	for _, raw := range expired {
		l.refuse(raw, refusedQueueTimeout)
	}
}

func (l *Listener) expireQueued() {
	now := time.Now()
	l.mu.Lock()
	var expired []net.Conn
	for l.qLen > 0 && !l.queue[l.qHead].deadline.After(now) {
		expired = append(expired, l.popQueued().conn)
	}
	l.armTimer(now)
	l.publish()
	l.mu.Unlock()
	for _, raw := range expired {
		l.refuse(raw, refusedQueueTimeout)
	}
}

// armTimer points the timer at the queue head's deadline. Every entry waits the same time, so the head always expires first.
func (l *Listener) armTimer(now time.Time) {
	if l.qLen == 0 {
		l.timer.Stop()
		return
	}
	l.timer.Reset(l.queue[l.qHead].deadline.Sub(now))
}

func (l *Listener) popQueued() queued {
	q := l.queue[l.qHead]
	l.queue[l.qHead] = queued{}
	l.qHead = (l.qHead + 1) % len(l.queue)
	l.qLen--
	return q
}
