//go:build goexperiment.jsonv2

package policy

// The allocation pins on the v2 JSON engine (GOEXPERIMENT=jsonv2, what
// build.env ships). Its Marshal costs one more allocation per call on tiny
// values, which the commit half's four small marshals show.
const (
	pinReserveKey     = 53
	pinReserveToken   = 58
	pinReserveNoRules = 20
	pinCommit         = 42
)
