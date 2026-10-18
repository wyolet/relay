//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret/keyring"
)

// The offline key ring commands write the control API's secret-key rows under a cli actor, refusals included, and report a failed audit write after the change it records.
func TestIntegration_SecretCommandsWriteAuditRows(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x33}, 32)
	dsn := storagetest.DB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("USER", "operator")
	st, err := storage.Open(ctx, dsn, storage.WithMigrateOnBoot(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := stored.New(st.Pool(), [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateScoped(ctx, "user:u1", "v", []byte("x")); err != nil {
		t.Fatal(err)
	}
	first, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := first.DataKeys[0].ID

	captureStdout(t, func() { err = runSecret([]string{"rotate", "--scope", "user:u1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if err := runSecret([]string{"rotate", "--scope", "user:nobody"}); !errors.Is(err, stored.ErrNoScopeKey) {
		t.Fatalf("rotate a scope with no key: %v", err)
	}
	if err := runSecret([]string{"destroy", oldKey, "--force"}); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("destroy a key in use: %v", err)
	}
	if _, err := b.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { err = runSecret([]string{"destroy", oldKey, "--force"}) })
	if err != nil {
		t.Fatal(err)
	}

	store := audit.NewStore(gen.New(st.Pool()))
	events, err := store.Events(ctx, audit.Query{ResourceKinds: []string{"secret-key"}})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		action, id, name string
		code             int
	}
	var got []row
	for _, ev := range events {
		if ev.Actor.Kind != audit.ActorCLI || ev.Actor.Name != "operator" || ev.Outcome.Status != audit.StatusAllowed {
			t.Errorf("actor or status of %s: %+v %+v", ev.Action, ev.Actor, ev.Outcome)
		}
		got = append(got, row{ev.Action, ev.Resource.ID, ev.Resource.Name, ev.Outcome.Code})
	}
	slices.Reverse(got)
	newKey := got[0].id
	want := []row{
		{"secret-keys.rotate", newKey, "user:u1", http.StatusOK},
		{"secret-keys.rotate", "", "user:nobody", http.StatusNotFound},
		{"secret-keys.destroy", oldKey, "", http.StatusConflict},
		{"secret-keys.destroy", oldKey, "user:u1", http.StatusOK},
	}
	if newKey == "" || newKey == oldKey || !slices.Equal(got, want) {
		t.Fatalf("audit rows:\n got %+v\nwant %+v", got, want)
	}

	// Without the audit table the rotation still happens, and the command fails so the operator knows the record is missing.
	if _, err := st.Pool().Exec(ctx, "ALTER TABLE audit_events RENAME TO audit_events_away"); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { err = runSecret([]string{"rotate", "--scope", "user:u1"}) })
	if err == nil || !strings.Contains(err.Error(), "audit log is missing") {
		t.Fatalf("rotate with the audit write failing: %v", err)
	}
	after, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.DataKeys) != 3 {
		t.Fatalf("the rotation did not happen: %+v", after.DataKeys)
	}
}
