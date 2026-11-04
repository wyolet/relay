package connadmit

import (
	"bytes"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxRefusers = 512
	// refuseHeadTimeout bounds the wait for the request head, so the reply isn't lost to a reset from unread request bytes.
	refuseHeadTimeout  = 250 * time.Millisecond
	refuseHeadBytes    = 4 << 10
	refuseWriteTimeout = time.Second
	// The drain after the half-close lets the client read the reply and close first; it is bounded so a client that keeps sending can't hold a refuser.
	refuseDrainBytes   = 64 << 10
	refuseDrainTimeout = time.Second
)

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
	buf := make([]byte, refuseHeadBytes)
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
