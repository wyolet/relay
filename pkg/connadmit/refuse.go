package connadmit

import (
	"bytes"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// maxRefusers sizes for a reconnect herd: clients that get no answer retry at once, so refusals must keep pace with arrivals. A refuser holds a goroutine and a pooled buffer, about 12 KiB.
	maxRefusers = 4096
	// refuseHeadTimeout bounds the wait for the request head, so the reply isn't lost to a reset from unread request bytes.
	refuseHeadTimeout  = 100 * time.Millisecond
	refuseHeadBytes    = 4 << 10
	refuseWriteTimeout = time.Second
	// The drain after the half-close lets the client read the reply and close first; it is bounded so a client that keeps sending can't hold a refuser.
	refuseDrainBytes   = 64 << 10
	refuseDrainTimeout = 250 * time.Millisecond
)

var refuseBufs = sync.Pool{New: func() any { b := make([]byte, refuseHeadBytes); return &b }}

var headEnd = []byte("\r\n\r\n")

// refuse answers raw with the 503 on a refuser goroutine, or closes it unanswered when maxRefusers are already busy. Never blocks.
func (l *Listener) refuse(raw net.Conn, reason prometheus.Counter) {
	select {
	case l.refusers <- struct{}{}:
		reason.Inc()
		go func() {
			defer func() { <-l.refusers }()
			answerRefusal(raw, l.refusal)
		}()
	default:
		refusedNoAnswer.Inc()
		_ = raw.Close()
	}
}

func answerRefusal(raw net.Conn, refusal []byte) {
	defer raw.Close()
	bp := refuseBufs.Get().(*[]byte)
	defer refuseBufs.Put(bp)
	buf := *bp
	_ = raw.SetReadDeadline(time.Now().Add(refuseHeadTimeout))
	for n := 0; n < len(buf); {
		m, err := raw.Read(buf[n:])
		n += m
		if err != nil || bytes.Contains(buf[:n], headEnd) {
			break
		}
	}
	_ = raw.SetWriteDeadline(time.Now().Add(refuseWriteTimeout))
	if _, err := raw.Write(refusal); err != nil {
		return
	}
	if cw, ok := raw.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = raw.SetReadDeadline(time.Now().Add(refuseDrainTimeout))
	for drained := 0; drained < refuseDrainBytes; {
		m, err := raw.Read(buf)
		drained += m
		if err != nil {
			return
		}
	}
}
