package telemetry

import (
	"context"
	"net/http"
	"time"
)

// HeaderContentCapture is the response header in which relay's receiver says whether it stores the content of this credential's calls: "store" (a governing policy stores it), "drop" (a policy or an operator switch does not), "client" (no policy governs; the client's opt-in decides). Mirrors relay's httpheader.HeaderContentCapture; this module imports nothing of the server.
const HeaderContentCapture = "X-WR-Content-Capture"

// The values of HeaderContentCapture.
const (
	ContentStore  = "store"
	ContentDrop   = "drop"
	ContentClient = "client"
)

// contentAnswer is the receiver's last answer and when it came. An empty value means the header was absent (another OTLP backend) or could not be learned.
type contentAnswer struct {
	value string
	at    time.Time
}

// SendsContent reports whether the content of a call recorded now would be sent, so a caller builds Content only then. Relay's answer decides when it gave one within the cache window: "store" sends even without the client's opt-in, "drop" never sends. Otherwise the opt-in (WithContent or the environment) decides.
func (e *Emitter) SendsContent() bool {
	if e.configErr != nil {
		return false
	}
	if a := e.answer.Load(); a != nil && time.Since(a.at) < e.answerTTL {
		switch a.value {
		case ContentStore:
			return true
		case ContentDrop:
			return false
		}
	} else {
		e.learnSoon()
	}
	return *e.cfg.content
}

func (e *Emitter) answerFresh() bool {
	a := e.answer.Load()
	return a != nil && time.Since(a.at) < e.answerTTL
}

// learnSoon asks the export goroutine to learn the answer when it is unknown or expired. Never blocks.
func (e *Emitter) learnSoon() {
	if e.answerFresh() {
		return
	}
	select {
	case e.probes <- struct{}{}:
	default:
	}
}

// learn sends an empty logs export, a valid OTLP request that records nothing, to read the answer from its response. A failed probe is cached like an absent header, so an unreachable server is not probed per batch; the next successful export replaces it.
func (e *Emitter) learn(ctx context.Context) {
	body, err := gzipJSON(exportLogs{ResourceLogs: []resourceLogs{}})
	if err != nil {
		return
	}
	status, header, err := e.send(ctx, logsPath, body)
	if err != nil || status/100 != 2 {
		e.answer.Store(&contentAnswer{at: time.Now()})
		return
	}
	e.learnFrom(header)
}

// learnFrom caches the answer of a successful export response.
func (e *Emitter) learnFrom(h http.Header) {
	e.answer.Store(&contentAnswer{value: h.Get(HeaderContentCapture), at: time.Now()})
}
