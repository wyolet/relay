package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Emitter exports recorded calls in the background. Build it once per process with New and share it; call Close before the process exits so queued calls are sent.
type Emitter struct {
	cfg       config
	configErr error
	resource  resource
	scope     scope

	// mu makes Close and Record exclusive, so no call is queued after the final drain.
	mu      sync.RWMutex
	closed  bool
	queue   chan *record
	flushes chan flushRequest
	probes  chan struct{}
	// stop is closed by Close; done is closed when run has returned, after setting closeErr.
	stop     chan struct{}
	done     chan struct{}
	closeErr error
	// ctx bounds every export; Close cancels it when its own ctx ends first, so a hanging server cannot keep run alive.
	ctx    context.Context
	cancel context.CancelFunc

	dropped atomic.Uint64
	warning atomic.Pointer[string]
	answer  atomic.Pointer[contentAnswer]

	// Retry and cache timings; fields so tests can shorten them.
	backoff      time.Duration
	maxRetryWait time.Duration
	answerTTL    time.Duration
}

type flushRequest struct {
	ctx  context.Context
	done chan error
}

// New returns an Emitter configured by opts, then by the environment (see the Env constants). Missing or malformed configuration does not panic: Err reports it, Record drops, and Flush returns it.
func New(opts ...Option) *Emitter {
	e := &Emitter{
		backoff:      time.Second,
		maxRetryWait: 30 * time.Second,
		answerTTL:    5 * time.Minute,
	}
	for _, o := range opts {
		o(&e.cfg)
	}
	if e.configErr = e.cfg.resolve(); e.configErr != nil {
		return e
	}
	version := sdkVersion()
	e.resource = resource{Attributes: []keyValue{
		stringAttr("service.name", e.cfg.serviceName),
		stringAttr("telemetry.sdk.name", "wyolet-relay-sdk"),
		stringAttr("telemetry.sdk.language", "go"),
	}}
	if version != "" {
		e.resource.Attributes = append(e.resource.Attributes, stringAttr("telemetry.sdk.version", version))
	}
	e.scope = scope{Name: sdkModule + "/telemetry", Version: version}
	e.queue = make(chan *record, e.cfg.queueSize)
	e.flushes = make(chan flushRequest)
	e.probes = make(chan struct{}, 1)
	e.stop = make(chan struct{})
	e.done = make(chan struct{})
	e.ctx, e.cancel = context.WithCancel(context.Background())
	go e.run()
	// Learning starts now, so relay's answer is usually known before the first call decides on its content.
	e.learnSoon()
	return e
}

// Err reports the configuration error the Emitter was built with, nil when it exports.
func (e *Emitter) Err() error { return e.configErr }

// Record queues one call for export. It never blocks: when the queue is full, or the Emitter is closed or misconfigured, the call is dropped and counted in Dropped. Record copies what it needs from c before it returns.
func (e *Emitter) Record(c Call) {
	if e.configErr != nil {
		e.dropped.Add(1)
		return
	}
	r := newRecord(&c)
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- r:
	default:
		e.dropped.Add(1)
	}
	e.learnSoon()
}

// Dropped reports how many reports were lost: calls refused by a full queue, a closed or misconfigured Emitter, and spans or content events whose export failed.
func (e *Emitter) Dropped() uint64 { return e.dropped.Load() }

// LastWarning returns the last message an export response carried in its partial success, "" when none did. Relay uses it, for example, to say content was not stored.
func (e *Emitter) LastWarning() string {
	if w := e.warning.Load(); w != nil {
		return *w
	}
	return ""
}

// Flush exports every call recorded so far and returns the first export error, or the configuration error.
func (e *Emitter) Flush(ctx context.Context) error {
	if e.configErr != nil {
		return e.configErr
	}
	return e.request(ctx, e.flushes)
}

// Close exports what is queued, then stops the Emitter; later calls are dropped. It returns when the queue is sent, with the first export error, or when ctx ends: then the export in flight is abandoned, what is still queued is dropped and counted, and the export goroutine exits all the same.
func (e *Emitter) Close(ctx context.Context) error {
	if e.configErr != nil {
		return e.configErr
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.stop)
	e.mu.Unlock()
	select {
	case <-e.done:
		return e.closeErr
	case <-ctx.Done():
		e.cancel()
		return ctx.Err()
	}
}

func (e *Emitter) request(ctx context.Context, to chan flushRequest) error {
	req := flushRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case to <- req:
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the export goroutine: it batches queued calls and exports a batch when it is full, when the interval passes, and on Flush and Close.
func (e *Emitter) run() {
	defer close(e.done)
	defer e.cancel()
	ticker := time.NewTicker(e.cfg.batchInterval)
	defer ticker.Stop()
	batch := make([]*record, 0, e.cfg.batchSize)
	for {
		select {
		case r := <-e.queue:
			if batch = append(batch, r); len(batch) >= e.cfg.batchSize {
				_ = e.export(e.ctx, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				_ = e.export(e.ctx, batch)
				batch = batch[:0]
			}
		case <-e.probes:
			if !e.answerFresh() {
				e.learn(e.ctx)
			}
		case req := <-e.flushes:
			ctx, stop := e.bounded(req.ctx)
			req.done <- e.drain(ctx, batch)
			stop()
			batch = batch[:0]
		case <-e.stop:
			// The final drain runs under the emitter's own context: Close waits for it until its ctx ends, then cancels it, so the drain is as long as Close lets it be.
			e.closeErr = e.drain(e.ctx, batch)
			return
		}
	}
}

// bounded returns ctx, also cancelled when Close gives up on the emitter.
func (e *Emitter) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	unregister := context.AfterFunc(e.ctx, cancel)
	return ctx, func() {
		unregister()
		cancel()
	}
}

// drain exports batch and everything queued behind it, in batches.
func (e *Emitter) drain(ctx context.Context, batch []*record) error {
	var first error
	for {
		select {
		case r := <-e.queue:
			if batch = append(batch, r); len(batch) < e.cfg.batchSize {
				continue
			}
		default:
		}
		if len(batch) == 0 {
			return first
		}
		if err := e.export(ctx, batch); err != nil && first == nil {
			first = err
		}
		if len(batch) < e.cfg.batchSize {
			return first
		}
		batch = batch[:0]
	}
}
