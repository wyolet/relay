package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

const secretUsage = "usage: relay secret status | migrate | downgrade"

// runSecret implements `relay secret status | migrate | downgrade`. They talk to Postgres directly with the deployment's RELAY_PG_DSN and master keys, so they work with every relay server stopped, which migrate and downgrade expect.
func runSecret(args []string) error {
	if len(args) != 1 {
		return errors.New(secretUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.PGDSN == "" {
		return errors.New("RELAY_PG_DSN is required")
	}
	ctx := context.Background()
	// The schema is left as found: downgrade runs before the schema itself is rolled back.
	st, err := storagemod.Open(ctx, cfg.PGDSN, storagemod.WithMigrateOnBoot(false))
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer st.Close()
	sec, err := appsecret.Wire(st.Pool(), cfg.MasterKeys())
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		status, err := sec.Stored.Status(ctx)
		if err != nil {
			return err
		}
		return printJSON(status)
	case "migrate":
		res, err := sec.Stored.RewriteStale(ctx)
		if err != nil {
			return err
		}
		status, err := sec.Stored.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("rewrote %d stored secrets (%d changed concurrently, %d failed); %d legacy rows left\n",
			res.Rewritten, res.Changed, res.Failed, status.LegacyRows)
		if res.Failed > 0 {
			return fmt.Errorf("%d stored secrets could not be decrypted with the configured master keys; see the log", res.Failed)
		}
		return nil
	case "downgrade":
		if cfg.MasterKey == nil {
			return errors.New("RELAY_MASTER_KEY is required: downgraded values are encrypted under it")
		}
		n, err := sec.Stored.Downgrade(ctx, cfg.MasterKey)
		fmt.Printf("rewrote %d stored secrets to the legacy shape under RELAY_MASTER_KEY\n", n)
		if err != nil {
			return err
		}
		fmt.Println("schema 34 can now be rolled back: relay migrate down 33")
		return nil
	}
	return errors.New(secretUsage)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
