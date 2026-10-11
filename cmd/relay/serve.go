package main

import (
	"net"
	"net/http"
)

// serve binds srv.Addr before it returns, so the caller learns the bound address, and serves in the background. A bind failure arrives on the channel the way a serve failure does.
func serve(srv *http.Server) (net.Addr, <-chan error) {
	errs := make(chan error, 1)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		errs <- err
		return nil, errs
	}
	go func() { errs <- srv.Serve(ln) }()
	return ln.Addr(), errs
}
