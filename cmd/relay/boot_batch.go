package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/batch"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/jobq"
	"github.com/wyolet/relay/jobq/payload"
)

func routingOptions(cfg *config.Config) []routing.Option {
	// RBAC makes a credential's grants the whole access model, so a key whose
	// policy does not resolve has no access rather than the shared pool's.
	var routingOpts []routing.Option
	if cfg.Authz == config.AuthzRBAC {
		routingOpts = append(routingOpts, routing.RequirePolicy())
	}
	return routingOpts
}

func buildBatch(bootCtx, listenerCtx context.Context, st *storagemod.Storage, cat *appcatalog.Catalog, pl *pipeline.Pipeline,
	specRegistry *adapter.Registry, routingOpts []routing.Option, maxItems int) (*jobq.Queue, *batch.Service) {
	// Batch subsystem: jobq-backed background execution of bulk inference
	// submissions. jobq owns durable per-item execution + payload storage;
	// app/batch owns the batch record and the customer API. The per-item
	// handler reuses the same routing + pipeline as the realtime path.
	if err := jobq.Migrate(bootCtx, st.Pool()); err != nil {
		slog.Error("jobq migrate failed", "err", err)
		os.Exit(1)
	}
	batchPayloadDir := os.Getenv("RELAY_BATCH_PAYLOAD_DIR")
	if batchPayloadDir == "" {
		batchPayloadDir = "relay-batch-payloads"
	}
	batchPayloads, err := payload.NewFileStore(batchPayloadDir)
	if err != nil {
		slog.Error("batch payload store init failed", "err", err)
		os.Exit(1)
	}
	batchQueue := jobq.New(st.Pool(), batchPayloads, jobq.Options{})
	batchSvc := batch.NewService(
		batch.NewStore(st.Pool()),
		batchQueue,
		&batch.Runner{Resolver: routing.New(cat, routingOpts...), Pipeline: pl, Specs: specRegistry, Catalog: cat},
		batchCaller,
		maxItems,
	)
	batchQueue.Register(batch.Queue, batchSvc.Handler())
	if err := batchQueue.Start(listenerCtx); err != nil {
		slog.Error("batch queue start failed", "err", err)
		os.Exit(1)
	}
	slog.Info("batch: subsystem started", "payload_dir", batchPayloadDir)
	return batchQueue, batchSvc
}
