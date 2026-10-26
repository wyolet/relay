//go:build !goexperiment.jsonv2

package policy

// The allocation pins on the classic JSON engine, for a build without
// build.env's GOEXPERIMENT (a bare go test).
const (
	pinReserveKey     = 29
	pinReserveToken   = 32
	pinReserveNoRules = 18
	pinCommit         = 14
)
