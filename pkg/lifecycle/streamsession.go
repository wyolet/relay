package lifecycle

import (
	"bytes"
	"log/slog"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewStreamSession builds a fresh observer per registered factory for one
// streamed request. Returns nil when no factories are registered (the
// caller skips driving a session). Feed each upstream frame to Observe,
// then call Finish at end-of-stream.
func (r *Registry) NewStreamSession(lc *Context) *StreamSession {
	r.mu.RLock()
	facs := r.streamFactories
	r.mu.RUnlock()
	if len(facs) == 0 || lc == nil {
		return nil
	}
	obs := make([]namedObserver, 0, len(facs))
	for _, f := range facs {
		obs = append(obs, namedObserver{name: f.Name(), o: f.NewObserver(lc)})
	}
	return &StreamSession{lc: lc, obs: obs, summ: v1.NewStreamSummarizer(lc.Translator)}
}

// StreamSession drives the per-request stream observers for one streamed
// response. Not safe for concurrent use — one stream, one goroutine.
//
// Frames reach the session one of two ways, never both for the same request:
// callers with a frame boundary already in hand call Observe; the runner
// tees the raw upstream body into the session as an io.Writer (Write), which
// reframes on the SSE blank-line separator and Observes each frame. Either
// way the session runs a StreamSummarizer alongside the observers so the
// runner can commit rate-limit tokens without re-parsing a buffered body.
type StreamSession struct {
	lc   *Context
	obs  []namedObserver
	summ *v1.StreamSummarizer

	// buf accumulates tee'd bytes (Write path) until a full SSE frame
	// (blank-line terminated) can be split off. Holds at most one partial
	// frame — this is the bounded state that replaces the old full-stream
	// tee buffer.
	buf      []byte
	finished bool
}

// maxPartialFrameBytes bounds the tee reframing buffer. A well-formed SSE
// stream keeps it at one partial frame, but the bound must hold for inputs
// that never produce a separator — a JSON error body on a streamed request,
// an SSE dialect we failed to detect, a hostile upstream. On overflow the
// buffer is fed as a best-effort frame (the translator either parses or
// rejects it) and reset, so memory stays bounded no matter what arrives.
const maxPartialFrameBytes = 1 << 20

// Write is the io.Writer the runner tees the raw upstream body into. It
// reframes the byte stream on the SSE blank-line separator — "\n\n" as sent
// by every major vendor, or the spec-legal CRLF form "\r\n\r\n" — and feeds
// each complete frame to the observers + summarizer, retaining only the
// trailing partial frame (bounded by maxPartialFrameBytes). CRLF frames are
// normalized to LF before feeding so downstream parsing sees one dialect.
// Never errors (always reports len(p) consumed).
func (s *StreamSession) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.buf = append(s.buf, p...)
	for {
		i, sep := bytes.Index(s.buf, []byte("\n\n")), 2
		if j := bytes.Index(s.buf, []byte("\r\n\r\n")); j >= 0 && (i < 0 || j < i) {
			i, sep = j, 4
		}
		if i < 0 {
			break
		}
		frame := s.buf[:i]
		if sep == 4 {
			frame = bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n"))
		}
		if len(bytes.TrimSpace(frame)) > 0 {
			s.feed(frame)
		}
		s.buf = s.buf[i+sep:]
	}
	if len(s.buf) > maxPartialFrameBytes {
		s.feed(s.buf)
		s.buf = nil
	}
	return len(p), nil
}

// feed hands one raw upstream SSE frame (separator stripped) to every
// observer and the summarizer.
func (s *StreamSession) feed(frame []byte) {
	s.summ.Observe(frame)
	for _, no := range s.obs {
		func() {
			defer recoverLifecycle(s.lc, "stream observer Observe")
			no.o.Observe(frame)
		}()
	}
}

type namedObserver struct {
	name string
	o    StreamObserver
}

// Observe feeds one upstream frame (separator stripped) to every observer
// and the summarizer. Nil-safe so callers can hold a nil *StreamSession (no
// factories) and call unconditionally. Callers using the Write (tee) path
// must NOT also call Observe — that would double-count frames.
func (s *StreamSession) Observe(frame []byte) {
	if s == nil {
		return
	}
	s.feed(frame)
}

// Finish closes the session: any buffered partial frame is flushed, each
// observer's Result is attached to the Context under its name (the Registry
// is still the sole writer), the rate-limit tokens the summarizer extracted
// are stashed for the runner, and the request is marked filled so the
// post-flight Finalize reuses the same collection instead of re-producing
// it. Idempotent — the echo response-writer Finishes early to splice usage,
// then the runner's post-flight Finishes again (a no-op). Nil-safe.
func (s *StreamSession) Finish() {
	if s == nil || s.lc == nil || s.finished {
		return
	}
	s.finished = true
	if len(bytes.TrimSpace(s.buf)) > 0 {
		s.feed(s.buf)
	}
	s.buf = nil
	for _, no := range s.obs {
		v := safeResult(no, s.lc)
		if v != nil {
			s.lc.attach(no.name, v)
		}
	}
	s.lc.setStreamTokens(map[string]int64(s.summ.Summary().Tokens))
	s.lc.filled = true
}

func safeResult(no namedObserver, lc *Context) (out any) {
	defer func() {
		if v := recover(); v != nil {
			slog.Error("lifecycle: stream observer Result panic recovered",
				"observer", no.name, "request_id", lc.RequestID, "panic", v)
			out = nil
		}
	}()
	v, err := no.o.Result()
	if err != nil {
		slog.Warn("lifecycle: stream observer Result error",
			"observer", no.name, "request_id", lc.RequestID, "err", err)
		return nil
	}
	return v
}
