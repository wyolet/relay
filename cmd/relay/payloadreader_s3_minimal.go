//go:build minimal

package main

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/secret"
)

// newS3PayloadReader is the minimal-build stub: the s3 backend (and its
// cloud SDK) is not compiled in. Reading with backend "s3" selected errors.
func newS3PayloadReader(_ context.Context, _ settings.PayloadLogging, _ *secret.Registry) (payloadlog.Reader, error) {
	return nil, errors.New(`payloadlog: s3 backend not compiled in (minimal build) — use backend "file"`)
}
