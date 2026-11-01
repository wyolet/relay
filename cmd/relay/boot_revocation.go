package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/wyolet/relay/app/revocation"
	"github.com/wyolet/relay/pkg/kv"
)

// revocationPoll bounds how long a token revoked on another pod keeps working here.
const revocationPoll = time.Second

// buildRevocations loads the revoked-token list and keeps this pod's copy current until ctx ends.
func buildRevocations(ctx context.Context, store kv.Store) *revocation.List {
	l := revocation.New(store)
	if err := l.Refresh(ctx); err != nil {
		slog.Warn("revocation: initial load failed; retrying on the poll", "err", err)
	}
	go l.Run(ctx, revocationPoll)
	return l
}
