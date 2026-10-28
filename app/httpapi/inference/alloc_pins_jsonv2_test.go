//go:build goexperiment.jsonv2

package inference

// pinTokenColdCache is the cold-cache token path's allocation pin on the v2
// JSON engine (GOEXPERIMENT=jsonv2, what build.env ships), whose claims decode
// allocates less.
const pinTokenColdCache = 12
