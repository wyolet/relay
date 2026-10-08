//go:build integration

// audit_session_test.go checks that the session id an audit row carries — as
// stored and as returned by GET /api/audit — never works as a session cookie.

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

const sessionCookie = "relay_session"

// auditedSession logs a user in, makes one audited write, and returns the
// session cookie value plus that write's audit row as GET /api/audit returns
// it (decoded and raw).
func auditedSession(t *testing.T, s *stack, username string) (string, audit.Event, []byte) {
	t.Helper()
	const password = "pw-session-id-12345"
	s.seedLogin(t, username, password)
	us := s.login(t, username, password)
	base, _ := url.Parse(s.control.URL)
	var token string
	for _, c := range us.client.Jar.Cookies(base) {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("login set no session cookie")
	}
	if code, raw := us.do(http.MethodPost, "/api/teams", `{"metadata":{"name":"`+username+`-team"},"spec":{}}`); code != http.StatusCreated {
		t.Fatalf("create team = %d: %s", code, raw)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		code, raw := s.adminDo(http.MethodGet, "/api/audit?action=teams.create&actor_name="+username, "")
		if code != http.StatusOK {
			t.Fatalf("GET /api/audit = %d: %s", code, raw)
		}
		var body auditListBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode audit list: %v", err)
		}
		if len(body.Events) == 1 {
			return token, body.Events[0], raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("no teams.create row for %s after 15s", username)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// authenticatesAsCookie reports whether value, presented as the session
// cookie, gets past GET /api/auth/whoami.
func authenticatesAsCookie(t *testing.T, s *stack, value string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.control.URL+"/api/auth/whoami", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// stringLeaves collects every string value in a decoded JSON document.
func stringLeaves(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case []any:
		for _, e := range x {
			stringLeaves(e, out)
		}
	case map[string]any:
		for _, e := range x {
			stringLeaves(e, out)
		}
	}
}

func TestAudit_SessionIDDoesNotAuthenticate(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	token, ev, raw := auditedSession(t, s, "auditee")

	if !authenticatesAsCookie(t, s, token) {
		t.Fatal("the live session cookie does not authenticate; the replay checks below would prove nothing")
	}
	if ev.Actor.Kind != audit.ActorUser || ev.Actor.SessionID == "" {
		t.Fatalf("actor = %+v, want a user with a session id", ev.Actor)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("GET /api/audit response contains the session token")
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var returned []string
	stringLeaves(doc, &returned)
	for _, v := range returned {
		if v != "" && authenticatesAsCookie(t, s, v) {
			t.Fatalf("returned audit value %q authenticates as the session", v)
		}
	}

	var stored *string
	if err := testPool(t, s.dsn).QueryRow(context.Background(),
		`SELECT session_id FROM audit_events WHERE id = $1`, ev.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored == nil || *stored != ev.Actor.SessionID {
		t.Fatalf("stored session_id = %v, want %q", stored, ev.Actor.SessionID)
	}
	if *stored == token || authenticatesAsCookie(t, s, *stored) {
		t.Fatalf("stored session_id %q authenticates as the session", *stored)
	}
}

// A row stored before the fix holds the raw token; the migration must turn it
// into the same id the session layer now stamps, and leave such ids alone.
func TestAudit_SessionIDMigrationReplacesStoredTokens(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	token, ev, _ := auditedSession(t, s, "legacy")

	legacy := ev
	legacy.ID = "01950000-0000-7000-8000-00000000cccc"
	legacy.Actor.SessionID = token
	ctx := context.Background()
	if err := s.audit.Write(ctx, []audit.Event{legacy}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	up, err := pgmigrations.FS.ReadFile("000031_audit_session_id_hash.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	pool := testPool(t, s.dsn)
	for range 2 {
		if _, err := pool.Exec(ctx, string(up)); err != nil {
			t.Fatalf("apply migration: %v", err)
		}
	}

	for _, id := range []string{legacy.ID, ev.ID} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT session_id FROM audit_events WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read row %s: %v", id, err)
		}
		if got != ev.Actor.SessionID {
			t.Fatalf("row %s session_id = %q, want %q", id, got, ev.Actor.SessionID)
		}
	}
	if authenticatesAsCookie(t, s, ev.Actor.SessionID) {
		t.Fatal("migrated session_id authenticates as the session")
	}
}
