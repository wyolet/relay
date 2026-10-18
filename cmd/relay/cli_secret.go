package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/wyolet/relay/app/audit"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
)

// runSecret implements `relay secret <command>` (see secretUsage). The commands talk to Postgres directly with the deployment's RELAY_PG_DSN and master keys, so they work with every relay server stopped, which migrate and downgrade expect; rotate, rewrap and destroy also run against a live fleet, which learns of the change through the database. Every command but status writes audit rows under a cli actor.
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
	if cmd.name == "status" {
		status, err := sec.Stored.Status(ctx)
		if err != nil {
			return err
		}
		return printJSON(status)
	}
	rows, err := runSecretChange(ctx, sec.Stored, cfg.MasterKey, cmd)
	return errors.Join(err, writeSecretAudit(ctx, st.Pool(), cmd, rows))
}

// runSecretChange runs a command that changes stored secrets or the key ring, through the same backend methods as the control API's /secret-keys, and returns the audit rows describing what it did or why it was refused.
func runSecretChange(ctx context.Context, b *stored.Backend, masterKey []byte, cmd secretCommand) ([]audit.Row, error) {
	switch cmd.name {
	case "migrate":
		res, err := b.RewriteStale(ctx)
		if err == nil {
			err = reportMigrate(ctx, b, res)
		}
		return []audit.Row{secretAuditRow("secret-keys.migrate", "", rowCount(res.Rewritten), err)}, err
	case "downgrade":
		if masterKey == nil {
			err := fmt.Errorf("%w: downgraded values are encrypted under it", stored.ErrNoMasterKey)
			return []audit.Row{secretAuditRow("secret-keys.downgrade", "", rowCount(0), err)}, err
		}
		n, err := b.Downgrade(ctx, masterKey)
		fmt.Printf("rewrote %d stored secrets to the legacy shape under RELAY_MASTER_KEY\n", n)
		if err == nil {
			fmt.Println("schema 34 can now be rolled back: relay migrate down 33")
		}
		return []audit.Row{secretAuditRow("secret-keys.downgrade", "", rowCount(n), err)}, err
	case "rotate":
		return runRotate(ctx, b, cmd)
	case "rewrap":
		res, err := b.Rewrap(ctx, cmd.to)
		rows := []audit.Row{secretAuditRow("secret-keys.rewrap", "", cmd.to, err)}
		if err != nil {
			return rows, err
		}
		if err := printJSON(res); err != nil {
			return rows, err
		}
		if len(res.Skipped) > 0 {
			return rows, fmt.Errorf("%d data keys were left under another master key, which this process does not hold; rerun with both that key and the target configured", len(res.Skipped))
		}
		return rows, nil
	case "destroy":
		k, err := b.Destroy(ctx, cmd.id, cmd.force)
		if err != nil {
			return []audit.Row{secretAuditRow("secret-keys.destroy", cmd.id, "", err)}, err
		}
		fields := []string{"state"}
		if cmd.force {
			fields = append(fields, "force")
		}
		return []audit.Row{secretAuditRow("secret-keys.destroy", k.ID, k.Scope, nil, fields...)}, printJSON(k)
	}
	return nil, errors.New(secretUsage)
}

func runRotate(ctx context.Context, b *stored.Backend, cmd secretCommand) ([]audit.Row, error) {
	var keys []stored.DataKeyStatus
	var err error
	if cmd.all {
		keys, err = b.RotateAll(ctx)
	} else {
		var k stored.DataKeyStatus
		if k, err = b.Rotate(ctx, cmd.scope); err == nil {
			keys = []stored.DataKeyStatus{k}
		}
	}
	rows := make([]audit.Row, 0, len(keys)+1)
	for _, k := range keys {
		rows = append(rows, secretAuditRow("secret-keys.rotate", k.ID, k.Scope, nil))
	}
	// As in the API, the attempt itself gets a row when it failed or changed nothing.
	if err != nil || len(keys) == 0 {
		name := cmd.scope
		if cmd.all {
			name = "*"
		}
		rows = append(rows, secretAuditRow("secret-keys.rotate", "", name, err))
	}
	if err != nil {
		if len(keys) > 0 {
			_ = printJSON(keys)
		}
		return rows, err
	}
	if err := printJSON(append([]stored.DataKeyStatus{}, keys...)); err != nil {
		return rows, err
	}
	fmt.Fprintln(os.Stderr, "running servers rewrite the stored secrets under the new keys; with none running, run `relay secret migrate`")
	return rows, nil
}

func reportMigrate(ctx context.Context, b *stored.Backend, res stored.RewriteResult) error {
	status, err := b.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("rewrote %d stored secrets (%d changed concurrently, %d failed); %d legacy rows left\n",
		res.Rewritten, res.Changed, res.Failed, status.LegacyRows)
	if res.Failed > 0 {
		return fmt.Errorf("%d stored secrets could not be decrypted with the configured master keys; see the log", res.Failed)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
