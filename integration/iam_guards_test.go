//go:build integration

// iam_guards_test.go covers the guards that only show up against a real
// Postgres: the 0026 backfill's owner matching and its down leg, the key
// hash uniqueness the snapshot's hash index depends on, the built-in role
// upsert, and the apply endpoint's refusal body.
package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

// A key whose owner names its user by the YAML slug (the username) is that
// user's key, not an orphan to re-parent onto a generated account.
func TestIntegration_BackfillMatchesUsernameOwners(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	u := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	m := migrator(t)
	if err := m.Migrate(25); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 25: %v", err)
	}
	p := testPool(t)

	// A name long enough that "legacy-" + name overflows a 63-char slug.
	longName := strings.Repeat("k", 70)
	for name, owner := range map[string]string{
		"by-username": `{"owner":{"kind":"user","id":"alice"}}`,
		longName:      `{"owner":{"kind":"user"}}`,
	} {
		if _, err := p.Exec(ctx,
			`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
			 VALUES ($1, $2, '', $3, $4::jsonb, '{}'::jsonb)`,
			ids.New(), name, sha256Hex(name), owner); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate up to 26: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("migrate back up to head: %v", err)
		}
	})

	var userID *string
	if err := p.QueryRow(ctx,
		`SELECT principal_user_id FROM relay_keys WHERE name = 'by-username'`).Scan(&userID); err != nil {
		t.Fatalf("read by-username: %v", err)
	}
	if userID == nil || *userID != u.ID {
		t.Errorf("username-owned key principal = %v, want %q", userID, u.ID)
	}

	// The generated account name fits the slug column.
	var saName string
	if err := p.QueryRow(ctx,
		`SELECT sa.name FROM relay_keys k JOIN service_accounts sa ON sa.id = k.principal_sa_id
		  WHERE k.name = $1`, longName).Scan(&saName); err != nil {
		t.Fatalf("read generated account: %v", err)
	}
	if len(saName) != 63 {
		t.Errorf("generated account name is %d chars (%q), want it truncated to 63", len(saName), saName)
	}

	// The down leg leaves every row with an owner the pre-migration code
	// accepts: a Key was always user-owned there.
	if err := m.Migrate(25); err != nil {
		t.Fatalf("migrate down to 25: %v", err)
	}
	rows, err := p.Query(ctx, `SELECT name, metadata->'owner'->>'kind' FROM relay_keys`)
	if err != nil {
		t.Fatalf("read owners: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if kind != "user" {
			t.Errorf("after the down migration key %q has owner kind %q, want user", name, kind)
		}
	}
}

// Only a token_version change has to reach the snapshot; every other column
// on users would rebuild it for nothing.
func TestIntegration_UsersNotifyOnlyOnTokenVersion(t *testing.T) {
	newStack(t)
	ctx := context.Background()
	p := testPool(t)

	var triggers []string
	rows, err := p.Query(ctx,
		`SELECT tgname FROM pg_trigger WHERE tgrelid = 'users'::regclass AND NOT tgisinternal ORDER BY tgname`)
	if err != nil {
		t.Fatalf("read triggers: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		triggers = append(triggers, name)
	}
	if len(triggers) != 2 {
		t.Fatalf("triggers on users = %v, want the write and version pair", triggers)
	}
	var def string
	if err := p.QueryRow(ctx,
		`SELECT pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE tgrelid = 'users'::regclass AND tgname = 'users_notify_version'`).Scan(&def); err != nil {
		t.Fatalf("read trigger def: %v", err)
	}
	if !strings.Contains(def, "token_version") {
		t.Errorf("update trigger has no token_version condition: %s", def)
	}
}

// A key hash lives on the snapshot's hash index alongside every other key's
// current and pre-rotation hash: two rows claiming one hash would hand one
// key the other's traffic.
func TestIntegration_KeyHashCannotBeClaimedTwice(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	projectID, saID := st.seedProjectAndAccount("hash-guard")

	mk := func(name, hash, prev string) *key.Key {
		k := &key.Key{Meta: meta.Metadata{
			ID: ids.New(), Name: name,
			Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID},
		}}
		k.Spec.Principal = key.Principal{Kind: key.PrincipalServiceAccount, ID: saID}
		k.Spec.KeyHash = hash
		k.Spec.PreviousKeyHash = prev
		return k
	}

	first := mk("first", sha256Hex("first"), "")
	if err := st.stores.Key.Upsert(ctx, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	// Rotating leaves the old hash live for the grace window.
	first.Spec.PreviousKeyHash = first.Spec.KeyHash
	first.Spec.KeyHash = sha256Hex("first-rotated")
	if err := st.stores.Key.Upsert(ctx, first); err != nil {
		t.Fatalf("rotate first: %v", err)
	}

	for _, hash := range []string{first.Spec.KeyHash, first.Spec.PreviousKeyHash} {
		if err := st.stores.Key.Upsert(ctx, mk("second", hash, "")); err == nil {
			t.Fatalf("a second key claimed hash %q", hash)
		}
	}
	// Its own hashes are not "in use" by anyone else.
	if err := st.stores.Key.Upsert(ctx, first); err != nil {
		t.Fatalf("re-upserting the same key: %v", err)
	}
}

// A rule added to a built-in role in a release has to reach a deployment
// that already seeded that role.
func TestIntegration_BuiltinRolesUpsertChangedRules(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	seeded := seedBuiltinRoles(t, st)
	admin := seeded["team-admin"]

	stripped := *admin
	stripped.Spec.Rules = []role.Rule{{Kinds: []string{"usage"}, Verbs: []string{"read"}}}
	if err := st.stores.Role.Upsert(ctx, &stripped); err != nil {
		t.Fatalf("write the stale row: %v", err)
	}

	if err := role.SeedBuiltins(ctx, st.stores.Role, slog.Default(), nil); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got, err := st.stores.Role.Get(ctx, admin.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.Allows("tokens", "revoke") {
		t.Errorf("stale rules survived the re-seed: %+v", got.Spec.Rules)
	}
	if got.Meta.ID != admin.Meta.ID {
		t.Errorf("the role's id changed: %q -> %q", admin.Meta.ID, got.Meta.ID)
	}
}

// A refusal must not name the row it refused: the plan is withheld from a
// caller who may not write it, and the reason would leak the same state.
func TestIntegration_ApplyForbiddenBodyIsGeneric(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	seedBuiltinRoles(t, st)
	password := "correct-horse"
	st.seedLogin(t, "carol", password)
	us := st.login(t, "carol", password)

	doc := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata:\n  name: platform\nspec: {}\n"
	code, raw := us.doAs(http.MethodPost, "/api/apply", doc, "application/yaml")
	if code != http.StatusForbidden {
		t.Fatalf("apply as an unbound user = %d: %s", code, raw)
	}
	var body struct {
		Message string          `json:"message"`
		Plan    json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Message != "forbidden" {
		t.Errorf("message = %q, want a generic forbidden", body.Message)
	}
	if strings.Contains(string(raw), "platform") {
		t.Errorf("the body names the refused row: %s", raw)
	}
}

// A rotation replaces the stored hash; without the dirty flag the next apply
// of the manifest that declared the key writes the old hash back.
func TestIntegration_KeyRotateMarksTheRowDirty(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	projectID, saID := st.seedProjectAndAccount("rotate-dirty")

	k := &key.Key{Meta: meta.Metadata{
		ID: ids.New(), Name: "ci", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID},
	}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalServiceAccount, ID: saID}
	k.Spec.KeyHash = sha256Hex("ci")
	if err := st.stores.Key.Upsert(ctx, k); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	code, raw := st.adminDo(http.MethodPost, "/api/keys/by-id/"+k.Meta.ID+"/rotate", "")
	if code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", code, raw)
	}
	got, err := st.stores.Key.Get(ctx, k.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.Meta.Dirty {
		t.Error("a rotated key is not marked dirty; the next apply would revert it")
	}
}

// Team, Group and Role rows are system-owned by rule, so an export that
// filtered on provenance alone would ship a bundle missing the tenancy the
// operator authored.
func TestIntegration_ExportIncludesSystemOwnedTenancy(t *testing.T) {
	st := newStack(t)
	st.mkTeam(t, "platform")
	if code, raw := st.adminDo(http.MethodPost, "/api/groups",
		`{"metadata":{"name":"platform-eng"},"spec":{}}`); code != http.StatusCreated {
		t.Fatalf("POST /api/groups = %d: %s", code, raw)
	}

	req, _ := http.NewRequest(http.MethodGet, st.control.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer "+st.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"kind: Team", "name: platform", "kind: Group", "name: platform-eng"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("export is missing %q:\n%s", want, body)
		}
	}
	// Built-in roles stay out: they are relay's own rows.
	if strings.Contains(string(body), "name: team-admin") {
		t.Errorf("export ships a built-in role:\n%s", body)
	}
}
