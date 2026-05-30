//go:build !minimal

package main

import (
	"context"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	payloads3 "github.com/wyolet/relay/pkg/payload/s3"
	"github.com/wyolet/relay/pkg/secret"
)

// newS3PayloadReader builds the object-store payload reader, resolving the
// access/secret keys through the secret registry — mirroring the s3 sink
// seam so reads target the same bucket/prefix writes go to. Present in the
// default build; the minimal build provides a stub that errors.
func newS3PayloadReader(ctx context.Context, cfg settings.PayloadLogging, resolver *secret.Registry) (payloadlog.Reader, error) {
	ak, err := resolveOptionalRef(ctx, resolver, cfg.S3.AccessKey)
	if err != nil {
		return nil, err
	}
	sk, err := resolveOptionalRef(ctx, resolver, cfg.S3.SecretKey)
	if err != nil {
		return nil, err
	}
	return payloads3.NewReader(ctx, payloads3.Config{
		Endpoint:  cfg.S3.Endpoint,
		Bucket:    cfg.S3.Bucket,
		Region:    cfg.S3.Region,
		Prefix:    cfg.S3.Prefix,
		UseSSL:    cfg.S3.UseSSL,
		AccessKey: string(ak),
		SecretKey: string(sk),
	})
}
