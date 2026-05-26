package main

import (
	"context"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/internal/config"
	payloadfile "github.com/wyolet/relay/pkg/payload/file"
)

// buildPayloadSink selects the payload storage backend. The file backend
// is always compiled in; the s3 backend is provided by a build-tagged
// seam (newS3PayloadSink) so a "minimal" build omits its cloud SDK
// entirely — Go module pruning keeps minio-go out of that binary.
func buildPayloadSink(ctx context.Context, cfg *config.Config) (payloadlog.Sink, error) {
	switch cfg.PayloadLogBackend {
	case "s3":
		return newS3PayloadSink(ctx, cfg)
	default: // "file"
		return payloadfile.NewSink(cfg.PayloadLogPath)
	}
}
