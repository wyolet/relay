//go:build !goexperiment.jsonv2

package policy

// The allocation pins on the classic JSON engine, for a build without
// build.env's GOEXPERIMENT (a bare go test).
const (
	pinReserveKey     = 54
	pinReserveToken   = 59
	pinReserveNoRules = 21
	pinCommit         = 38
)
