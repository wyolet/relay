package main

import (
	"context"
	"log/slog"

	"github.com/wyolet/relay/app/lastused"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// buildLastUsed records which relay keys and host keys serve, as a lifecycle
// collector, and flushes the times onto their rows from one goroutine per pod.
// It returns a stop that waits for the final flush. RELAY_LAST_USED=off
// records nothing.
func buildLastUsed(ctx context.Context, cfg *config.Config, st *storagemod.Storage, lifecycleReg *lifecycle.Registry) func() {
	if !cfg.LastUsed {
		return func() {}
	}
	store := lastused.NewStore(gen.New(st.Pool()))
	rec := lastused.NewRecorder(lastused.DefaultMaxEntries)
	lifecycleReg.RegisterCollector(rec)
	flusher := lastused.NewFlusher(rec, store, lastused.DefaultFlushInterval, slog.Default())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		flusher.Run(runCtx)
	}()
	slog.Debug("lastused: collector wired", "flush_interval", lastused.DefaultFlushInterval)
	return func() {
		cancel()
		<-done
	}
}
