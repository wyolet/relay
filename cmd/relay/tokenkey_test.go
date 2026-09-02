package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// settingsRow fakes the single-row pgx.Row GetSetting scans — enough to
// drive settings.Store off no live Postgres at all.
type settingsRow struct {
	section string
	value   []byte
}

func (r settingsRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.section
	*dest[1].(*[]byte) = r.value
	*dest[2].(*pgtype.Timestamptz) = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return nil
}

// stubDBTX answers QueryRow with a fixed settings row. loadTokenSigningKey's
// path through settings.Store.Get never calls Exec/Query/CopyFrom, so those
// just satisfy gen.DBTX and panic if a future change starts using them.
type stubDBTX struct{ row settingsRow }

func (s stubDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unused by settings.Store.Get")
}
func (s stubDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unused by settings.Store.Get")
}
func (s stubDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return s.row
}
func (s stubDBTX) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unused by settings.Store.Get")
}

// loadTokenSigningKey must not fail boot when the stored signing-key ref
// can no longer be resolved (rotated master key, deleted secret row) — it
// should log a warning, clear the signer and verifier, and let the gateway
// come up with tokens disabled (mint then answers 503).
func TestLoadTokenSigningKey_UnresolvableRefDisablesTokensInsteadOfFailingBoot(t *testing.T) {
	badRef := pkgsecret.Ref{Kind: pkgsecret.KindStored, ID: "does-not-exist"}
	cfg := settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour, SigningKey: badRef}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}

	db := stubDBTX{row: settingsRow{section: settings.AuthTokensSection, value: raw}}
	stores := &appcatalog.Stores{
		Settings: settings.NewStore(gen.New(db)),
		// No backend registered for "stored": resolving the ref fails
		// exactly like a rotated master key or a deleted secret row would.
		Secrets: pkgsecret.NewRegistry(),
	}

	signer := &control.TokenSigner{}
	verifier := &inference.TokenVerifier{}

	if err := loadTokenSigningKey(context.Background(), nil, stores, nil, signer, verifier); err != nil {
		t.Fatalf("loadTokenSigningKey returned %v, want nil — an unusable key must not fail boot", err)
	}
	if pub := signer.PublicKey(); pub != nil {
		t.Errorf("signer has a public key %x after an unresolvable ref, want none", pub)
	}
}
