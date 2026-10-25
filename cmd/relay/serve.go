package main

import (
	"math"
	"net"
	"net/http"
	"runtime/debug"

	"golang.org/x/net/netutil"
)

// serve binds srv.Addr before it returns, so the caller learns the bound address, and serves in the background. A bind failure arrives on the channel the way a serve failure does. maxConns > 0 caps the open connections: past it, new ones wait in the kernel's accept queue until one closes.
func serve(srv *http.Server, maxConns int) (net.Addr, <-chan error) {
	errs := make(chan error, 1)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		errs <- err
		return nil, errs
	}
	if maxConns > 0 {
		ln = netutil.LimitListener(ln, maxConns)
	}
	go func() { errs <- srv.Serve(ln) }()
	return ln.Addr(), errs
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
