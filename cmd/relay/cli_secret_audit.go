package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/internal/storage/gen"
)

// secretAuditRow is the row the control API records for the same action, with the status it would have answered err with.
func secretAuditRow(action, id, name string, err error, fields ...string) audit.Row {
	row := control.SecretKeyAuditRow(action, id, name, fields...)
	row.Code = http.StatusOK
	if err != nil {
		row.Code = control.SecretKeyErrorStatus(err)
	}
	return row
}

// rowCount names the stored secrets a migrate or downgrade converted.
func rowCount(n int) string { return fmt.Sprintf("%d rows", n) }

// writeSecretAudit writes rows under a cli actor named by the OS user. Its error reaches the operator after the command's own outcome: the change it records has already happened.
func writeSecretAudit(ctx context.Context, pool *pgxpool.Pool, cmd secretCommand, rows []audit.Row) error {
	actor := audit.Actor{Kind: audit.ActorCLI, Name: osUser()}
	req := audit.Request{Path: "relay secret " + cmd.name}
	if err := audit.WriteRows(ctx, audit.NewStore(gen.New(pool)), actor, req, rows); err != nil {
		return fmt.Errorf("the audit log is missing the record of this command: %w", err)
	}
	return nil
}

func osUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
