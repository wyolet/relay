package main

import (
	"context"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	payloadfile "github.com/wyolet/relay/pkg/payload/file"
	"github.com/wyolet/relay/pkg/secret"
)

// payloadSinkBuilder returns the payloadlog.SinkBuilder the Controller calls
// on each reconcile. The file backend is always compiled in; the s3 backend
// is provided by a build-tagged seam (newS3PayloadSink) so a "minimal" build
// omits its cloud SDK — Go module pruning keeps minio-go out of that binary.
// S3 credentials are secret.Refs resolved through the shared registry.
func payloadSinkBuilder(resolver *secret.Registry) payloadlog.SinkBuilder {
	return func(ctx context.Context, cfg settings.PayloadLogging) (payloadlog.Sink, error) {
		switch cfg.Backend {
		case "s3":
			return newS3PayloadSink(ctx, cfg, resolver)
		default: // "file"
			path := cfg.File.Path
			if path == "" {
				path = "relay-payloads.jsonl"
			}
			return payloadfile.NewSink(path)
		}
	}
}
