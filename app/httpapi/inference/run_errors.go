package inference

import (
	"errors"
	"net/http"

	"github.com/wyolet/relay/app/adapter"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// mapRunErr answers a failed pipeline run for a request in inbound's shape. Limits the state store could not check are relay's own overload, answered like a shed and retried after the same pause.
func mapRunErr(w http.ResponseWriter, inbound *adapter.Spec, err error) {
	if errors.Is(err, pkgratelimit.ErrUnavailable) {
		writeOverloaded(w, overloadedFor(inbound))
		return
	}
	mapPipelineErr(w, err)
}
