//go:build !minimal

package main

import (
	"context"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	payloads3 "github.com/wyolet/relay/pkg/payload/s3"
	"github.com/wyolet/relay/pkg/secret"
)

// newS3PayloadSink builds the object-store payload backend, resolving the
// access/secret keys through the secret registry. An empty Ref means
// ambient credentials (e.g. an IAM role) — the minio client falls back to
// its own credential chain. Present in the default build; the minimal
// build provides a stub that errors.
func newS3PayloadSink(ctx context.Context, cfg settings.PayloadLogging, resolver *secret.Registry) (payloadlog.Sink, error) {
	ak, err := resolveOptionalRef(ctx, resolver, cfg.S3.AccessKey)
	if err != nil {
		return nil, err
	}
	sk, err := resolveOptionalRef(ctx, resolver, cfg.S3.SecretKey)
	if err != nil {
		return nil, err
	}
	return payloads3.New(ctx, payloads3.Config{
		Endpoint:  cfg.S3.Endpoint,
		Bucket:    cfg.S3.Bucket,
		Region:    cfg.S3.Region,
		Prefix:    cfg.S3.Prefix,
		UseSSL:    cfg.S3.UseSSL,
		AccessKey: string(ak),
		SecretKey: string(sk),
	})
}

// resolveOptionalRef resolves ref, treating a zero Ref as "unset" (nil).
func resolveOptionalRef(ctx context.Context, resolver *secret.Registry, ref secret.Ref) ([]byte, error) {
	if ref.Kind == "" {
		return nil, nil
	}
	return resolver.Resolve(ctx, ref)
}
