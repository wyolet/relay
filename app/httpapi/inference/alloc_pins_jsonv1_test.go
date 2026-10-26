//go:build !goexperiment.jsonv2

package inference

// pinTokenColdCache is the cold-cache token path's allocation pin on the
// classic JSON engine, for a build without build.env's GOEXPERIMENT (a bare
// go test).
const pinTokenColdCache = 20
