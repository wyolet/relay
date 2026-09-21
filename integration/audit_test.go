//go:build integration

// audit_test.go covers the admin audit log against real Postgres: rows
// written for mutations, the GET /api/audit filters and keyset pagination,
// and the retention prune.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
)

type auditListBody struct {
	Events     []audit.Event `json:"events"`
	NextCursor string        `json:"next_cursor"`
}

// auditList calls GET /api/audit with a raw query string.
func (s *stack) auditList(query string) auditListBody {
	s.t.Helper()
	code, raw := s.adminDo(http.MethodGet, "/api/audit"+query, "")
	if code != http.StatusOK {
		s.t.Fatalf("GET /api/audit%s = %d: %s", query, code, raw)
	}
	var out auditListBody
	if err := json.Unmarshal(raw, &out); err != nil {
		s.t.Fatalf("decode audit list: %v: %s", err, raw)
	}
	return out
}

// waitForAuditRows polls until at least n rows are readable — the emitter
// batches on a 1s tick, so a write is not visible synchronously.
func (s *stack) waitForAuditRows(n int) []audit.Event {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		evs := s.auditList("?limit=1000").Events
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("only %d audit rows after 15s, want %d", len(evs), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAudit_MutationsRecordedAndFiltered(t *testing.T) {
	s := newStack(t)

	code, raw := s.adminDo(http.MethodPost, "/api/teams", `{"metadata":{"name":"audited","displayName":"Audited Team"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("create team = %d: %s", code, raw)
	}
	var created struct {
		Metadata struct{ ID string } `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	if code, raw := s.adminDo(http.MethodDelete, "/api/teams/by-id/"+created.Metadata.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete team = %d: %s", code, raw)
	}

	evs := s.waitForAuditRows(2)

	// An allowed read (the polling GET /api/audit itself) must not appear.
	for _, ev := range evs {
		if ev.Action == "audit.read" {
			t.Fatalf("an allowed read produced a row: %+v", ev)
		}
	}

	byAction := map[string]audit.Event{}
	for _, ev := range evs {
		byAction[ev.Action] = ev
	}
	create, ok := byAction["teams.create"]
	if !ok {
		t.Fatalf("no teams.create row in %d events", len(evs))
	}
	if create.Outcome.Status != audit.StatusAllowed || create.Outcome.Code != http.StatusCreated {
		t.Fatalf("create outcome = %+v, want allowed/201", create.Outcome)
	}
	if create.Actor.Kind != audit.ActorAdminToken {
		t.Fatalf("create actor = %+v, want admin-token", create.Actor)
	}
	if create.Change == nil || len(create.Change.Fields) != 1 || create.Change.Fields[0] != "*" {
		t.Fatalf("create change = %+v, want [*]", create.Change)
	}
	if create.Request.Method != http.MethodPost || create.Request.Path != "/api/teams" {
		t.Fatalf("create request = %+v", create.Request)
	}
	if _, ok := byAction["teams.delete"]; !ok {
		t.Fatalf("no teams.delete row in %d events", len(evs))
	}

	// Filters.
	if got := s.auditList("?action=teams.create").Events; len(got) != 1 || got[0].Action != "teams.create" {
		t.Fatalf("action filter returned %d rows: %+v", len(got), got)
	}
	if got := s.auditList("?resource_kind=team").Events; len(got) != 2 {
		t.Fatalf("resource_kind filter returned %d rows, want 2", len(got))
	}
	if got := s.auditList("?resource_kind=policy").Events; len(got) != 0 {
		t.Fatalf("resource_kind=policy returned %d rows, want 0", len(got))
	}
	if got := s.auditList("?status=denied").Events; len(got) != 0 {
		t.Fatalf("status=denied returned %d rows, want 0", len(got))
	}
	if got := s.auditList("?from=" + time.Now().UTC().Add(time.Hour).Format(time.RFC3339)).Events; len(got) != 0 {
		t.Fatalf("future from= returned %d rows, want 0", len(got))
	}
}

func TestAudit_KeysetPagination(t *testing.T) {
	s := newStack(t)

	const n = 5
	for i := 0; i < n; i++ {
		if code, raw := s.adminDo(http.MethodPost, "/api/teams", fmt.Sprintf(`{"metadata":{"name":"paged-%d","displayName":"Paged Team"},"spec":{}}`, i)); code != http.StatusCreated {
			t.Fatalf("create team %d = %d: %s", i, code, raw)
		}
	}
	s.waitForAuditRows(n)

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < n; page++ {
		q := "?limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		body := s.auditList(q)
		if len(body.Events) != 1 {
			t.Fatalf("page %d returned %d rows, want 1", page, len(body.Events))
		}
		id := body.Events[0].ID
		if seen[id] {
			t.Fatalf("page %d repeated row %s", page, id)
		}
		seen[id] = true
		if body.NextCursor == "" {
			t.Fatalf("page %d had no next_cursor with %d rows still to come", page, n-page-1)
		}
		cursor = body.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("paged over %d distinct rows, want %d", len(seen), n)
	}
}

func TestAudit_RetentionPruneRemovesOnlyExpiredRows(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	old := audit.Event{
		ID: "01950000-0000-7000-8000-00000000aaaa", TS: time.Now().UTC().AddDate(0, 0, -400),
		Actor: audit.Actor{Kind: audit.ActorAdminToken}, Action: "teams.create",
		Resource: audit.Resource{Kind: "team"},
		Outcome:  audit.Outcome{Status: audit.StatusAllowed, Code: 201},
	}
	fresh := old
	fresh.ID, fresh.TS = "01950000-0000-7000-8000-00000000bbbb", time.Now().UTC()
	if err := s.audit.Write(ctx, []audit.Event{old, fresh}); err != nil {
		t.Fatalf("seed audit rows: %v", err)
	}

	n, err := s.audit.Prune(ctx, time.Now().UTC().AddDate(0, 0, -365))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	left, err := s.audit.Events(ctx, audit.Query{Limit: 100})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(left) != 1 || left[0].ID != fresh.ID {
		t.Fatalf("remaining rows = %+v, want only the fresh one", left)
	}
}
