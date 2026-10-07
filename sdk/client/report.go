package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/wyolet/relay/sdk/telemetry"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// WithTelemetry reports every call the client makes to e, for clients that call a provider directly (For, ForFrom, OpenAI, OpenAIResponses, Anthropic, Gemini). On a relay client (Relay, RelayWS) it is a configuration error surfaced on the first call: relay records those calls itself, and reporting them too would count them twice.
func WithTelemetry(e *telemetry.Emitter) Option { return func(c *Client) { c.telemetry = e } }

// Telemetry returns the emitter the client reports to, nil when it reports nothing. Use it to Flush or read Dropped on the emitter ForMode built.
func (c *Client) Telemetry() *telemetry.Emitter { return c.telemetry }

var errTelemetryThroughRelay = errors.New("relay client: telemetry cannot be on when calls go through relay")

// report is one call being recorded for telemetry. A nil *report means telemetry is off, and every method is a no-op.
type report struct {
	emitter *telemetry.Emitter
	call    telemetry.Call
	// sent is set once the request reached the transport; a call that failed before that is not a model call.
	sent bool
	// Stream state: the output items seen, the terminal finish reason, and whether the generation completed.
	output    []v1.Item
	finish    v1.FinishReason
	completed bool
	ended     bool
}

func (c *Client) startReport(ctx context.Context, start time.Time, stream bool) *report {
	if c.telemetry == nil || c.configErr != nil {
		return nil
	}
	provider := c.providerName
	if p := c.target.binding.Providers; len(p) > 0 {
		provider = p[0]
	}
	return &report{emitter: c.telemetry, call: telemetry.Call{
		Operation:      telemetry.OperationChat,
		Provider:       provider,
		Host:           c.target.host,
		ServerAddress:  c.host(),
		Stream:         stream,
		Start:          start,
		Parent:         telemetry.TraceparentFromContext(ctx),
		ConversationID: telemetry.ConversationFromContext(ctx),
	}}
}

// requestSent notes the model name sent (the first of sent) and, when content may be sent, copies the input content while the caller still owns req.
func (r *report) requestSent(req *v1.Request, sent v1.ModelRefs) {
	if r == nil {
		return
	}
	r.sent = true
	if len(sent) > 0 {
		r.call.RequestModel = sent[0]
	}
	if r.emitter.SendsContent() {
		r.call.Content = telemetry.NewContent(req)
	}
}

// failed records a call that produced no response: an error status (err nil) or a transport, read or decode error.
func (r *report) failed(status int, err error) {
	if r == nil || !r.sent {
		return
	}
	r.call.StatusCode = status
	r.call.ErrorType = errorType(status, err)
	r.record()
}

func (r *report) succeeded(status int, resp *v1.Response) {
	if r == nil {
		return
	}
	r.call.StatusCode = status
	r.call.ResponseID, r.call.ResponseModel = resp.ID, resp.Model
	if resp.FinishReason != "" {
		r.call.FinishReasons = []string{string(resp.FinishReason)}
	}
	r.call.Usage = resp.Usage
	r.call.Content.SetOutput(resp.Output, resp.FinishReason)
	r.record()
}

func (r *report) record() {
	r.call.End = time.Now()
	r.emitter.Record(r.call)
}

// streamStarted notes the status of a stream that is about to be read.
func (r *report) streamStarted(status int) {
	if r != nil {
		r.call.StatusCode = status
	}
}

// observe reads what a canonical stream event tells the report.
func (r *report) observe(event string, data []byte) {
	if r == nil {
		return
	}
	switch event {
	case v1.EventGenerationCreated:
		var ev v1.GenerationCreatedEvent
		if json.Unmarshal(data, &ev) == nil {
			r.call.ResponseID, r.call.ResponseModel = ev.ID, ev.Model
		}
	case v1.EventItemCompleted:
		if r.call.Content == nil {
			return
		}
		var ev v1.ItemCompletedEvent
		if json.Unmarshal(data, &ev) == nil && ev.Item != nil {
			r.output = append(r.output, ev.Item)
		}
	case v1.EventGenerationCompleted:
		var ev v1.GenerationCompletedEvent
		if json.Unmarshal(data, &ev) == nil {
			r.finish, r.completed = ev.FinishReason, true
			if r.call.ResponseID == "" {
				r.call.ResponseID = ev.ID
			}
		}
	case v1.EventError:
		var ev v1.ErrorEvent
		if json.Unmarshal(data, &ev) == nil && ev.Code != "" {
			r.call.ErrorType = ev.Code
		} else {
			r.call.ErrorType = telemetry.ErrorOther
		}
	}
}

// streamEnded records a stream once, when it ends: at io.EOF (err nil), on a read error, or when the caller closes it (closed). A stream closed before its generation completed carries the usage seen so far and is marked canceled.
func (r *report) streamEnded(s *Stream, err error, closed bool) {
	if r == nil || r.ended {
		return
	}
	r.ended = true
	if s.firstByteSeen {
		r.call.TimeToFirstChunk = s.firstByte
	}
	r.call.Usage = s.usage
	switch {
	case err != nil:
		r.call.ErrorType = errorType(0, err)
	case closed && !r.completed:
		r.call.ErrorType = telemetry.ErrorCanceled
	}
	switch {
	case !r.completed:
		r.call.FinishReasons = []string{telemetry.FinishError}
	case r.finish != "":
		r.call.FinishReasons = []string{string(r.finish)}
	}
	r.call.Content.SetOutput(r.output, r.finish)
	r.record()
}

// errorType names a failure the way telemetry reports it: the HTTP error status, else a stable word for a timeout, a cancellation or a failed connection.
func errorType(status int, err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case status >= 400:
		return strconv.Itoa(status)
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return telemetry.ErrorTimeout
	case errors.Is(err, context.Canceled):
		return telemetry.ErrorCanceled
	case errors.As(err, &dnsErr), errors.As(err, &opErr) && opErr.Op == "dial":
		return telemetry.ErrorUnreachable
	}
	return telemetry.ErrorOther
}
