//go:build minimal

package main

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/internal/config"
)

// newS3PayloadSink is the minimal-build stub: the s3 backend (and its
// cloud SDK) is not compiled in. Selecting it errors at boot.
func newS3PayloadSink(_ context.Context, _ *config.Config) (payloadlog.Sink, error) {
	return nil, errors.New(`payloadlog: s3 backend not compiled in (minimal build) — use RELAY_PAYLOADLOG_BACKEND=file`)
}
