//go:build !minimal

package main

import (
	"context"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/internal/config"
	payloads3 "github.com/wyolet/relay/pkg/payload/s3"
)

// newS3PayloadSink constructs the object-store payload backend. Present in
// the default build; the minimal build provides a stub that errors.
func newS3PayloadSink(ctx context.Context, cfg *config.Config) (payloadlog.Sink, error) {
	return payloads3.New(ctx, payloads3.Config{
		Endpoint:  cfg.PayloadLogS3Endpoint,
		Bucket:    cfg.PayloadLogS3Bucket,
		Region:    cfg.PayloadLogS3Region,
		AccessKey: cfg.PayloadLogS3AccessKey,
		SecretKey: cfg.PayloadLogS3SecretKey,
		Prefix:    cfg.PayloadLogS3Prefix,
		UseSSL:    cfg.PayloadLogS3UseSSL,
	})
}
