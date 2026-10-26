//go:build goexperiment.jsonv2

package policy

// The allocation pins on the v2 JSON engine (GOEXPERIMENT=jsonv2, what
// build.env ships).
const (
	pinReserveKey     = 27
	pinReserveToken   = 30
	pinReserveNoRules = 16
	pinCommit         = 14
)
