package client

import (
	"github.com/wyolet/relay/sdk/catalog"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Response is a non-streaming generation result with optional catalog pricing.
type Response struct {
	*v1.Response
	binding catalog.Binding
	priced  bool
}

// Cost returns total cost from the target's pricing rate sheet. ok is false
// for relay targets, unpriced hosts, and clients built without catalog resolution.
func (r *Response) Cost() (float64, bool) {
	if r == nil || !r.priced {
		return 0, false
	}
	return r.binding.Cost(r.Usage)
}
