package session

import (
	"log/slog"
	"net/http"
)

// onceWriter guards the response writer under the session middleware. It absorbs duplicate WriteHeader calls, because scs relays them to the stdlib writer, whose "superfluous WriteHeader" warning names scs instead of the handler at fault; the offender's method and path are logged instead. After ErrorFunc has written an error response mid-request, it drops the handler's remaining body so the 500 payload is not followed by junk.
type onceWriter struct {
	http.ResponseWriter
	req    *http.Request
	status int
	wrote  bool
	failed bool
}

func (o *onceWriter) WriteHeader(code int) {
	if o.wrote {
		slog.Warn("duplicate WriteHeader suppressed",
			"method", o.req.Method, "path", o.req.URL.Path,
			"first", o.status, "second", code)
		return
	}
	o.wrote, o.status = true, code
	o.ResponseWriter.WriteHeader(code)
}

func (o *onceWriter) Write(b []byte) (int, error) {
	if o.failed {
		// Report success so the handler finishes harmlessly; the client only sees the error response.
		return len(b), nil
	}
	if !o.wrote {
		o.wrote, o.status = true, http.StatusOK
	}
	return o.ResponseWriter.Write(b)
}

// fail writes an error response if the header has not gone out yet, then swallows everything the handler writes afterwards.
func (o *onceWriter) fail(code int, contentType string, body []byte) {
	if !o.wrote {
		o.Header().Set("Content-Type", contentType)
		o.WriteHeader(code)
		_, _ = o.ResponseWriter.Write(body)
	}
	o.failed = true
}

func (o *onceWriter) Unwrap() http.ResponseWriter { return o.ResponseWriter }
