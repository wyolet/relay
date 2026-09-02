package audit

import (
	"net"
	"net/http"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// Middleware mints the in-flight event for every control-plane request and
// emits it once the response is written. Mount it after the session and
// admin-token middlewares so the actor is already in context. trusted is
// the proxy set X-Forwarded-For is honoured behind; nil records the peer.
func Middleware(e *Emitter, trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f := &inflight{
				actor:     actorOf(r, trusted),
				request:   Request{ID: reqid.From(r.Context()), Method: r.Method, Path: r.URL.Path},
				readRoute: isReadRoute(r.Method),
			}
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r.WithContext(withInflight(r.Context(), f)))
			if ev, ok := f.event(sw.code()); ok {
				e.Emit(ev)
			}
		})
	}
}

// actorOf projects the context actor onto the audit shape. An
// unauthenticated caller is anonymous, not absent — a denied or failed
// request is exactly what an audit log is for.
func actorOf(r *http.Request, trusted []*net.IPNet) Actor {
	a := actor.From(r.Context())
	out := Actor{Kind: ActorAnonymous, IP: httpmw.ClientIP(r, trusted)}
	switch {
	case a == nil:
	case a.UserID != "":
		out.Kind, out.ID, out.Name, out.SessionID = ActorUser, a.UserID, a.Username, a.SessionID
	case a.AdminToken:
		out.Kind, out.Name = ActorAdminToken, a.Username
	}
	return out
}

// statusWriter captures the response status. Nothing else in the control
// chain exposes one: session's onceWriter is unexported.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// code is the observed status, defaulting to 200 for a handler that wrote
// neither header nor body.
func (s *statusWriter) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}
