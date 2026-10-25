package ratelimit

import (
	"errors"
	"fmt"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

// ErrUnavailable is returned by Reserve when the store did not answer in time, so the request's limits could not be checked.
var ErrUnavailable = errors.New("limit: rate-limit store unavailable")

// reserveFailed maps a reserve script's failure. A store that did not answer refuses the request: admitting it would run it unmetered and, for a token, unchecked for revocation.
func reserveFailed(err error) error {
	if errors.Is(err, kv.ErrPoolBusy) {
		reserveShed.Inc()
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if errors.Is(err, kv.ErrUnavailable) {
		metrics.KVError("limit_reserve", metrics.KVRefused)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("limit: reserve script: %w", err)
}
