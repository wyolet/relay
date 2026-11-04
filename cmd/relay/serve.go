package main

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/connadmit"
	"github.com/wyolet/relay/pkg/httpheader"
)

// serve binds srv.Addr before it returns, so the caller learns the bound address, and serves in the background. A bind failure arrives on the channel the way a serve failure does. A non-nil admission caps the open connections, queueing and refusing past the cap; with CloseIdle it takes over srv.ConnState.
func serve(srv *http.Server, admission *connadmit.Config) (net.Addr, <-chan error) {
	errs := make(chan error, 1)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		errs <- err
		return nil, errs
	}
	if admission != nil {
		admitted := connadmit.New(ln, *admission)
		if admission.CloseIdle {
			srv.ConnState = admitted.ConnState
		}
		ln = admitted
	}
	go func() { errs <- srv.Serve(ln) }()
	return ln.Addr(), errs
}

// inferenceAdmission refuses connections past the cap with the answer the in-flight shed gives a request in relay's own shape.
func inferenceAdmission(cfg *config.Config) *connadmit.Config {
	body, _ := json.Marshal(httpapi.APIError{Err: httpapi.APIErrorBody{Type: "server_error", Code: "overloaded", Message: httpapi.OverloadMessage}})
	return &connadmit.Config{
		MaxConns:     inferenceMaxConns(cfg.MaxConns),
		QueueSize:    cfg.ConnQueue,
		QueueTimeout: cfg.ConnQueueTimeout,
		CloseIdle:    cfg.ConnCloseIdle,
		Body:         body,
		RetryAfter:   httpapi.RetryAfterShed,
		Header: http.Header{
			inference.HeaderOrigin:       {"relay"},
			httpheader.HeaderShouldRetry: {"true"},
		},
	}
}

// connBudgetBytes is the memory budgeted per open connection: one stopped mid-request holds about 31 KiB live (23 KiB heap, 8 KiB stack; an idle keep-alive one about 19 KiB), doubled for the garbage the heap carries between collections.
const connBudgetBytes = 64 << 10

// Open connections may take connMemoryShareNum/connMemoryShareDen (40 %) of the memory limit; the rest is for the requests in flight, the record queues and the runtime.
const (
	connMemoryShareNum = 2
	connMemoryShareDen = 5
)

const (
	minDerivedMaxConns = 1024
	// noMemLimitMaxConns applies when no memory limit is set, so the budget has nothing to divide.
	noMemLimitMaxConns = 16384
)

// inferenceMaxConns is RELAY_MAX_CONNS when set, else as many connections as fit in 40 % of the Go memory limit (GOMEMLIMIT) at connBudgetBytes each, so a connection flood can't run the pod out of memory.
func inferenceMaxConns(configured int) int {
	if configured > 0 {
		return configured
	}
	limit := debug.SetMemoryLimit(-1)
	if limit == math.MaxInt64 {
		return noMemLimitMaxConns
	}
	return max(minDerivedMaxConns, int(limit/connMemoryShareDen*connMemoryShareNum/connBudgetBytes))
}
