package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// runSecret implements `relay secret <command>` (see secretUsage). The commands talk to Postgres directly with the deployment's RELAY_PG_DSN and master keys, so they work with every relay server stopped, which migrate and downgrade expect; rotate, rewrap and destroy also run against a live fleet, which learns of the change through the database.
func runSecret(args []string) error {
	cmd, err := parseSecretArgs(args)
	if err != nil {
		return err
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
	switch cmd.name {
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
	return runSecretKeys(ctx, sec.Stored, cmd)
}

// runSecretKeys runs the key ring commands through the same backend methods as the control API's /secret-keys.
func runSecretKeys(ctx context.Context, b *stored.Backend, cmd secretCommand) error {
	switch cmd.name {
	case "rotate":
		var keys []stored.DataKeyStatus
		if cmd.all {
			var err error
			if keys, err = b.RotateAll(ctx); err != nil {
				if len(keys) > 0 {
					_ = printJSON(keys)
				}
				return err
			}
		} else {
			k, err := b.Rotate(ctx, cmd.scope)
			if err != nil {
				return err
			}
			keys = []stored.DataKeyStatus{k}
		}
		if err := printJSON(keys); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "running servers rewrite the stored secrets under the new keys; with none running, run `relay secret migrate`")
		return nil
	case "rewrap":
		res, err := b.Rewrap(ctx, cmd.to)
		if err != nil {
			return err
		}
		if err := printJSON(res); err != nil {
			return err
		}
		if len(res.Skipped) > 0 {
			return fmt.Errorf("%d data keys were left under another master key, which this process does not hold; rerun with both that key and the target configured", len(res.Skipped))
		}
		return nil
	case "destroy":
		k, err := b.Destroy(ctx, cmd.id, cmd.force)
		if err != nil {
			return err
		}
		return printJSON(k)
	}
	return errors.New(secretUsage)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
