//go:build minimal

package main

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/secret"
)

// newS3PayloadSink is the minimal-build stub: the s3 backend (and its cloud
// SDK) is not compiled in. Selecting backend "s3" errors at reconcile.
func newS3PayloadSink(_ context.Context, _ settings.PayloadLogging, _ *secret.Registry) (payloadlog.Sink, error) {
	return nil, errors.New(`payloadlog: s3 backend not compiled in (minimal build) — use backend "file"`)
}
