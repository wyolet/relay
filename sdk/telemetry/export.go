package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	tracesPath = "/v1/traces"
	logsPath   = "/v1/logs"
	// maxAttempts bounds how often one export is sent.
	maxAttempts = 3
	// maxResponseBytes bounds how much of an export response is read for its partial success.
	maxResponseBytes = 64 << 10
)

// export sends one batch: the spans, then the content events of the calls whose content may be sent. A failed export drops its records and counts them.
func (e *Emitter) export(ctx context.Context, batch []*record) error {
	if !e.answerFresh() {
		e.learn(ctx)
	}
	spans := make([]span, len(batch))
	for i, r := range batch {
		spans[i] = r.span()
	}
	err := e.post(ctx, tracesPath, exportTraces{ResourceSpans: []resourceSpans{{
		Resource:   e.resource,
		ScopeSpans: []scopeSpans{{Scope: e.scope, Spans: spans}},
	}}})
	if err != nil {
		e.dropped.Add(uint64(len(batch)))
	}
	if !e.SendsContent() {
		return err
	}
	var events []logRecord
	for _, r := range batch {
		if r.content != nil && len(r.content.attrs) > 0 {
			events = append(events, r.event())
		}
	}
	if len(events) == 0 {
		return err
	}
	if logsErr := e.post(ctx, logsPath, exportLogs{ResourceLogs: []resourceLogs{{
		Resource:  e.resource,
		ScopeLogs: []scopeLogs{{Scope: e.scope, LogRecords: events}},
	}}}); logsErr != nil {
		e.dropped.Add(uint64(len(events)))
		err = errors.Join(err, logsErr)
	}
	return err
}

// post sends one export request, retrying a throttled or unavailable server and network errors as OTLP/HTTP asks: honouring Retry-After, at most maxAttempts times and maxRetryWait in all.
func (e *Emitter) post(ctx context.Context, path string, payload any) error {
	body, err := gzipJSON(payload)
	if err != nil {
		return fmt.Errorf("telemetry: encode %s: %w", path, err)
	}
	var waited time.Duration
	for attempt := 1; ; attempt++ {
		status, header, err := e.send(ctx, path, body)
		if err == nil && status/100 == 2 {
			e.learnFrom(header)
			return nil
		}
		if err == nil {
			err = fmt.Errorf("telemetry: export %s: HTTP %d", path, status)
		}
		retryable := ctx.Err() == nil && (status == 0 || status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout)
		if !retryable || attempt >= maxAttempts {
			return err
		}
		wait := e.backoff << (attempt - 1)
		if after, ok := retryAfter(header.Get("Retry-After")); ok {
			wait = after
		}
		if waited+wait > e.maxRetryWait {
			return err
		}
		waited += wait
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// send makes one attempt. status is 0 when no response arrived.
func (e *Emitter) send(ctx context.Context, path string, body []byte) (int, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultExportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	for k, v := range e.cfg.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.cfg.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode/100 == 2 {
		e.readPartialSuccess(raw)
	}
	return resp.StatusCode, resp.Header, nil
}

// readPartialSuccess keeps the server's message when it sent one. Only JSON is read: the request was JSON, so a conforming server answers in JSON.
func (e *Emitter) readPartialSuccess(body []byte) {
	var resp struct {
		PartialSuccess struct {
			ErrorMessage string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.PartialSuccess.ErrorMessage != "" {
		msg := resp.PartialSuccess.ErrorMessage
		e.warning.Store(&msg)
	}
}

// retryAfter reads a Retry-After value in seconds or as an HTTP date.
func retryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, time.Until(t)), true
	}
	return 0, false
}

func gzipJSON(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
