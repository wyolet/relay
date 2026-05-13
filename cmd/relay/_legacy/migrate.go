package main

import (
	"fmt"
	"log"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/wyolet/relay/internal/config"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func runMigrate(args []string) {
	if len(args) == 0 {
		log.Fatal("usage: relay migrate <up|version>")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.PGDSN == "" {
		log.Fatal("RELAY_PG_DSN not set")
	}

	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		log.Fatalf("migrate: open source: %v", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, cfg.PGDSN)
	if err != nil {
		log.Fatalf("migrate: init: %v", err)
	}
	defer m.Close()

	switch args[0] {
	case "up":
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			log.Fatalf("migrate up: %v", err)
		}
		fmt.Println("migrate up: ok")
	case "version":
		ver, dirty, err := m.Version()
		if err != nil {
			log.Fatalf("migrate version: %v", err)
		}
		fmt.Printf("version=%d dirty=%v\n", ver, dirty)
	default:
		log.Fatalf("migrate: unknown subcommand %q", args[0])
	}
}
