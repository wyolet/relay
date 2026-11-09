package connadmit

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is a connection holding one of the listener's slots until it is closed.
type Conn struct {
	net.Conn
	l    *Listener
	once sync.Once
	idle atomic.Bool // written with l.mu held; read without it as a fast path

	// guarded by l.mu
	released   bool
	idleSince  time.Time
	prev, next *Conn
}

// Close closes the connection and frees its slot; later calls only close.
func (c *Conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c) })
	return err
}

// CloseWrite half-closes the connection when the inner one supports it, which net/http uses to avoid resetting a closing connection.
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ConnState tracks idle keep-alive connections; set it as the http.Server's ConnState.
func (l *Listener) ConnState(nc net.Conn, state http.ConnState) {
	c, ok := nc.(*Conn)
	if !ok || c.l != l {
		return
	}
	switch state {
	case http.StateIdle:
		now := time.Now()
		l.mu.Lock()
		if !c.released {
			l.unlinkIdle(c)
			l.linkIdle(c, now)
			l.publish()
		}
		l.mu.Unlock()
	case http.StateActive, http.StateHijacked, http.StateClosed:
		if !c.idle.Load() {
			return
		}
		l.mu.Lock()
		l.unlinkIdle(c)
		l.publish()
		l.mu.Unlock()
	}
}

// linkIdle appends c to the idle list. Since times only grow, the head stays the oldest.
func (l *Listener) linkIdle(c *Conn, now time.Time) {
	c.idleSince = now
	c.prev, c.next = l.idleTail, nil
	if l.idleTail != nil {
		l.idleTail.next = c
	} else {
		l.idleHead = c
	}
	l.idleTail = c
	l.idleLen++
	c.idle.Store(true)
}

func (l *Listener) unlinkIdle(c *Conn) {
	if !c.idle.Load() {
		return
	}
	if c.prev != nil {
		c.prev.next = c.next
	} else {
		l.idleHead = c.next
	}
	if c.next != nil {
		c.next.prev = c.prev
	} else {
		l.idleTail = c.prev
	}
	c.prev, c.next = nil, nil
	l.idleLen--
	c.idle.Store(false)
}
