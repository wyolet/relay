package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/jobq"
)

func runSubcommand() bool {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err := runMigrate(os.Args[2:]); err != nil {
				slog.Error("migrate failed", "err", err)
				os.Exit(1)
			}
			return true
		case "seed":
			if err := runSeed(os.Args[2:]); err != nil {
				slog.Error("seed failed", "err", err)
				os.Exit(1)
			}
			return true
		case "apply":
			runCLI("apply", runApply, os.Args[2:])
			return true
		case "export":
			runCLI("export", runExport, os.Args[2:])
			return true
		case "keygen":
			runCLI("keygen", runKeygen, os.Args[2:])
			return true
		case "token":
			runCLI("token", runToken, os.Args[2:])
			return true
		}
	}
	return false
}

func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config invalid", "err", err)
		os.Exit(1)
	}
	if cfg.PGDSN == "" {
		slog.Error("RELAY_PG_DSN required (new-arch boot is PG-only)")
		os.Exit(1)
	}
	return cfg
}

func openStorage(bootCtx context.Context, cfg *config.Config) *storagemod.Storage {
	if !cfg.MigrateOnBoot {
		slog.Warn("storage: boot migrations disabled (RELAY_MIGRATE_ON_BOOT=off); the schema is left as found")
	}
	st, err := storagemod.Open(bootCtx, cfg.PGDSN,
		storagemod.WithMaxConns(cfg.PGMaxConns),
		storagemod.WithMinConns(cfg.PGMinConns),
		storagemod.WithMigrateOnBoot(cfg.MigrateOnBoot))
	if err != nil {
		slog.Error("storage.Open failed", "err", err)
		os.Exit(1)
	}
	return st
}

func waitForStop(inferErr, ctrlErr <-chan error) int {
	exitCode := 0
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-quit:
		slog.Info("relay: received signal, shutting down", "signal", sig.String())
	case err := <-inferErr:
		if err != nil && err != http.ErrServerClosed {
			exitCode = 1
			slog.Error("relay inference: server error", "err", err)
		}
	case err := <-ctrlErr:
		if err != nil && err != http.ErrServerClosed {
			exitCode = 1
			slog.Error("relay control: server error", "err", err)
		}
	}
	return exitCode
}

func shutdown(cfg *config.Config, ctrlSrv, inferSrv *http.Server, cancelListener context.CancelFunc, batchQueue *jobq.Queue) {
	deadline := time.Duration(cfg.ShutdownDeadlineS) * time.Second
	if deadline == 0 {
		deadline = 15 * time.Second
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), deadline)
	defer shutCancel()
	if ctrlSrv != nil {
		_ = ctrlSrv.Shutdown(shutCtx)
	}
	_ = inferSrv.Shutdown(shutCtx)
	cancelListener()
	// Drain in-flight batch jobs so the graceful-requeue path can run;
	// without this the process exits mid-handler and interrupted jobs sit
	// `running` until the rescuer discards them (MaxAttempts=1).
	batchQueue.Wait()
}
