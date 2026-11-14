package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret"
)

// settingsRow fakes the single-row pgx.Row GetSetting scans, enough to drive settings.Store with no Postgres.
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

// stubDBTX answers QueryRow with a fixed settings row. settings.Store.Get calls nothing else, so the rest panic if a change starts using them.
type stubDBTX struct{ row settingsRow }

func (s stubDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unused by settings.Store.Get")
}
func (s stubDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unused by settings.Store.Get")
}
func (s stubDBTX) QueryRow(context.Context, string, ...any) pgx.Row { return s.row }
func (s stubDBTX) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unused by settings.Store.Get")
}

// seedResolver serves seeds by secret id and fails for the ids in broken.
type seedResolver struct {
	seeds  map[string][]byte
	broken map[string]bool
}

func (r seedResolver) Resolve(_ context.Context, ref secret.Ref) ([]byte, error) {
	if seed, ok := r.seeds[ref.ID]; ok && !r.broken[ref.ID] {
		return seed, nil
	}
	return nil, errors.New("seedResolver: no secret " + ref.ID)
}

func registryFor(r secret.Resolver) *secret.Registry {
	reg := secret.NewRegistry()
	reg.Register(secret.KindStored, r)
	return reg
}

// keyFromByte is a distinct Ed25519 seed per n, with its key pair.
func keyFromByte(n byte) ([]byte, ed25519.PrivateKey, ed25519.PublicKey) {
	s := make([]byte, ed25519.SeedSize)
	s[0] = n
	priv := ed25519.NewKeyFromSeed(s)
	return s, priv, priv.Public().(ed25519.PublicKey)
}

func tokenSection(current, previous string) settings.AuthTokens {
	a := settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
	if current != "" {
		a.SigningKey = secret.Ref{Kind: secret.KindStored, ID: current}
	}
	if previous != "" {
		a.PreviousSigningKey = secret.Ref{Kind: secret.KindStored, ID: previous}
	}
	return a
}

// storesFor holds cfg as the stored auth-tokens section, with secrets resolving its refs.
func storesFor(t *testing.T, cfg settings.AuthTokens, secrets *secret.Registry) *appcatalog.Stores {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &appcatalog.Stores{
		Settings: settings.NewStore(gen.New(stubDBTX{row: settingsRow{section: settings.AuthTokensSection, value: raw}})),
		Secrets:  secrets,
	}
}

// captureLog routes slog into a buffer for the test and resets the once-only master-key warning.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	noMasterKeyWarn = sync.Once{}
	t.Cleanup(func() {
		slog.SetDefault(prev)
		noMasterKeyWarn = sync.Once{}
	})
	return &buf
}

// verifies reports whether v accepts a token signed by priv.
func verifies(t *testing.T, v *inference.TokenVerifier, priv ed25519.PrivateKey) bool {
	t.Helper()
	pub, _ := priv.Public().(ed25519.PublicKey)
	tok, err := authtoken.Sign(priv, authtoken.KeyID(pub), crypto.TokenClaims{Sub: "user:u-1", Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(tok)
	return err == nil
}

// The signer mints under the current key; the verifier accepts the current and the retired key and nothing else, so a pod that never held the retired key still honours tokens minted before a rotation.
func TestApplyTokenSigningKey(t *testing.T) {
	curSeed, curPriv, curPub := keyFromByte(1)
	prevSeed, prevPriv, prevPub := keyFromByte(2)
	_, strangerPriv, _ := keyFromByte(3)
	oldSeed, _, oldPub := keyFromByte(9)
	seeds := map[string][]byte{"current": curSeed, "previous": prevSeed, "short": []byte("short")}
	ctx := context.Background()

	tests := []struct {
		name        string
		section     settings.AuthTokens
		broken      map[string]bool
		wantErr     string
		wantPub     ed25519.PublicKey
		wantPrev    ed25519.PublicKey
		wantWarning string
	}{
		{name: "current and previous", section: tokenSection("current", "previous"), wantPub: curPub, wantPrev: prevPub},
		// With no previous key named, the verifier keeps the key it is replacing as the retired one.
		{name: "current only", section: tokenSection("current", ""), wantPub: curPub, wantPrev: oldPub},
		{name: "no signing key clears both sides", section: tokenSection("", "previous")},
		{name: "current seed of the wrong length", section: tokenSection("short", ""), wantWarning: "the stored signing key is not an Ed25519 seed"},
		{name: "previous seed of the wrong length", section: tokenSection("current", "short"), wantPub: curPub, wantPrev: oldPub, wantWarning: "previous token signing key unusable"},
		{name: "current unresolvable", section: tokenSection("current", "previous"), broken: map[string]bool{"current": true},
			wantErr: "resolve signing key: ", wantPub: oldPub},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			signer, verifier := &control.TokenSigner{}, &inference.TokenVerifier{}
			// A key already installed shows whether the call replaced, cleared or left it.
			signer.SetSeed(oldSeed)
			verifier.SetKey(oldPub)

			err := applyTokenSigningKey(ctx, registryFor(seedResolver{seeds: seeds, broken: tc.broken}), &tc.section, signer, verifier)
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.HasPrefix(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want prefix %q", err, tc.wantErr)
			}
			if got := signer.PublicKey(); !got.Equal(tc.wantPub) {
				t.Errorf("signer public key = %x, want %x", got, tc.wantPub)
			}
			if tc.wantErr != "" {
				return
			}
			if got := verifier.PreviousPublicKey(); !got.Equal(tc.wantPrev) {
				t.Errorf("verifier previous key = %x, want %x", got, tc.wantPrev)
			}
			for who, c := range map[string]struct {
				priv ed25519.PrivateKey
				want bool
			}{"current": {curPriv, tc.wantPub != nil}, "previous": {prevPriv, tc.wantPrev.Equal(prevPub)}, "stranger": {strangerPriv, false}} {
				if got := verifies(t, verifier, c.priv); got != c.want {
					t.Errorf("token signed by the %s key verifies = %v, want %v", who, got, c.want)
				}
			}
			if tc.wantWarning == "" && logs.Len() != 0 || !strings.Contains(logs.String(), tc.wantWarning) {
				t.Errorf("log = %q, want %q", logs.String(), tc.wantWarning)
			}
		})
	}

	if err := applyTokenSigningKey(ctx, registryFor(seedResolver{}), nil, &control.TokenSigner{}, &inference.TokenVerifier{}); err != nil {
		t.Errorf("nil section: %v", err)
	}
}

// A previous key that fails to resolve once must be retried, not recorded as "there is no previous key", which would log out every bearer of a token minted before the rotation.
func TestApplyTokenSigningKey_TransientPreviousFailureKeepsTheKeySet(t *testing.T) {
	curSeed, _, curPub := keyFromByte(1)
	prevSeed, prevPriv, prevPub := keyFromByte(2)
	seeds := map[string][]byte{"current": curSeed, "previous": prevSeed}
	section := tokenSection("current", "previous")
	signer, verifier := &control.TokenSigner{}, &inference.TokenVerifier{}
	ctx := context.Background()
	logs := captureLog(t)

	if err := applyTokenSigningKey(ctx, registryFor(seedResolver{seeds: seeds}), &section, signer, verifier); err != nil {
		t.Fatal(err)
	}
	broken := registryFor(seedResolver{seeds: seeds, broken: map[string]bool{"previous": true}})
	if err := applyTokenSigningKey(ctx, broken, &section, signer, verifier); err != nil {
		t.Fatalf("apply with an unresolvable previous key: %v", err)
	}
	if !verifier.PreviousPublicKey().Equal(prevPub) || !signer.PublicKey().Equal(curPub) || !verifies(t, verifier, prevPriv) {
		t.Errorf("previous key = %x, want the one already installed", verifier.PreviousPublicKey())
	}
	if !strings.Contains(logs.String(), "previous token signing key unresolved; keeping the current key set") {
		t.Errorf("log = %q, want the unresolved-previous warning", logs.String())
	}
}

func TestLoadTokenSigningKey(t *testing.T) {
	curSeed, curPriv, curPub := keyFromByte(1)
	oldSeed, _, oldPub := keyFromByte(9)
	secrets := registryFor(seedResolver{seeds: map[string][]byte{"current": curSeed}})
	disabled := tokenSection("current", "")
	disabled.Enabled = false

	tests := []struct {
		name        string
		section     settings.AuthTokens
		secrets     *secret.Registry
		wantPub     ed25519.PublicKey // signer after the call; it starts with oldPub
		wantWarning string
	}{
		{"stored ref is installed", tokenSection("current", ""), secrets, curPub, ""},
		{"disabled section leaves the signer alone", disabled, secrets, oldPub, ""},
		// A rotated master key or a deleted secret row: boot must come up with tokens off (mint answers 503), not fail.
		{"unresolvable ref disables tokens", tokenSection("gone", ""), secret.NewRegistry(), nil, "inference tokens disabled — signing key unusable"},
		{"no key and no master key to store one under", tokenSection("", ""), secrets, oldPub, "no RELAY_MASTER_KEY to store a signing key under"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			signer, verifier := &control.TokenSigner{}, &inference.TokenVerifier{}
			signer.SetSeed(oldSeed)
			verifier.SetKey(oldPub)

			if err := loadTokenSigningKey(context.Background(), nil, storesFor(t, tc.section, tc.secrets), nil, signer, verifier); err != nil {
				t.Fatalf("loadTokenSigningKey = %v; an unusable key must not fail boot", err)
			}
			if got := signer.PublicKey(); !got.Equal(tc.wantPub) {
				t.Errorf("signer public key = %x, want %x", got, tc.wantPub)
			}
			if got := verifies(t, verifier, curPriv); got != tc.wantPub.Equal(curPub) {
				t.Errorf("token under the stored key verifies = %v", got)
			}
			if tc.wantWarning == "" && logs.Len() != 0 || !strings.Contains(logs.String(), tc.wantWarning) {
				t.Errorf("log = %q, want %q", logs.String(), tc.wantWarning)
			}
		})
	}
}

func TestApplyAuthTokensSection(t *testing.T) {
	curSeed, _, curPub := keyFromByte(1)
	secrets := registryFor(seedResolver{seeds: map[string][]byte{"current": curSeed}})
	ctx := context.Background()
	logs := captureLog(t)
	signer, verifier := &control.TokenSigner{}, &inference.TokenVerifier{}

	// Tokens switched on after boot have no signing key yet: the watcher takes the generate path, which without a master key warns. Boot and the watcher's first delivery both reach it; the warning is logged once.
	keyless := tokenSection("", "")
	stores := storesFor(t, keyless, secrets)
	if err := loadTokenSigningKey(ctx, nil, stores, nil, signer, verifier); err != nil {
		t.Fatal(err)
	}
	if err := applyAuthTokensSection(ctx, nil, stores, nil, keyless, signer, verifier); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "no RELAY_MASTER_KEY"); n != 1 || signer.PublicKey() != nil {
		t.Errorf("master-key warning logged %d times with signer key %x; want once and no key", n, signer.PublicKey())
	}

	withKey := tokenSection("current", "")
	if err := applyAuthTokensSection(ctx, nil, storesFor(t, withKey, secrets), nil, withKey, signer, verifier); err != nil || !signer.PublicKey().Equal(curPub) {
		t.Fatalf("section with a key: err %v, signer key %x", err, signer.PublicKey())
	}
	// Disabling clears both sides even though the section still names its key.
	withKey.Enabled = false
	if err := applyAuthTokensSection(ctx, nil, storesFor(t, withKey, secrets), nil, withKey, signer, verifier); err != nil || signer.PublicKey() != nil || verifier.PreviousPublicKey() != nil {
		t.Errorf("disabled section: err %v, signer key %x", err, signer.PublicKey())
	}
}

func TestRotateTokenSigningKey_NeedsAMasterKey(t *testing.T) {
	err := rotateTokenSigningKey(context.Background(), nil, storesFor(t, tokenSection("current", ""), secret.NewRegistry()), nil, &control.TokenSigner{}, &inference.TokenVerifier{})
	if err == nil || err.Error() != "no master key to store a signing key under" {
		t.Errorf("err = %v", err)
	}
}
