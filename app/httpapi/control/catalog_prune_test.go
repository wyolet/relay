package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalogprune"
	"github.com/wyolet/relay/pkg/meta"
)

// fakePrune records each call and answers with rep and err.
type fakePrune struct {
	rep   catalogprune.Report
	err   error
	calls []string
}

func (f *fakePrune) run(_ context.Context, idle time.Duration, apply bool) (catalogprune.Report, error) {
	mode := "dry-run"
	if apply {
		mode = "apply"
	}
	f.calls = append(f.calls, mode+" "+idle.String())
	return f.rep, f.err
}

func newPruneHarness(t *testing.T, prune *fakePrune) (http.Handler, *auditSink, *audit.Emitter) {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	r, api := testAPI(t, scopeActors, audit.Middleware(em, nil))
	d := Deps{Authz: audit.Authorizer{Inner: testRBAC()}}
	if prune != nil {
		d.CatalogPrune = prune.run
	}
	registerCatalogPrune(api, d, nil)
	return r, sink, em
}

var (
	pruneDetachedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	hostOwner       = meta.Owner{Kind: meta.OwnerHost, ID: "h-acme"}
	prunedBinding   = catalogprune.Decision{Kind: "host-binding", Name: "old-chat-on-acme", ID: "b-1", DetachedAt: pruneDetachedAt, Decision: catalogprune.Prune, Owner: meta.Owner{Kind: meta.OwnerSystem}}
	keptPolicy      = catalogprune.Decision{Kind: "policy", Name: "acme-tier-0", ID: "p-1", DetachedAt: pruneDetachedAt, Decision: catalogprune.Keep, Reason: "edited", Owner: hostOwner}
)

type pruneBody struct {
	DryRun       bool                    `json:"dryRun"`
	Idle         string                  `json:"idle"`
	Pruned       int                     `json:"pruned"`
	KeptDetached int                     `json:"keptDetached"`
	Rows         []catalogprune.Decision `json:"rows"`
}

// A prune without dryRun is a dry run: it answers the decisions, deletes
// nothing and leaves no audit row.
func TestCatalogPruneDryRunByDefault(t *testing.T) {
	prune := &fakePrune{rep: catalogprune.Report{Idle: 720 * time.Hour, Rows: []catalogprune.Decision{prunedBinding, keptPolicy}, Pruned: 1, Kept: 1}}
	h, sink, em := newPruneHarness(t, prune)
	w := scopeReq(t, h, "root", http.MethodPost, "/catalog/prune", "")
	var got pruneBody
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	want := pruneBody{DryRun: true, Idle: "720h0m0s", Pruned: 1, KeptDetached: 1, Rows: []catalogprune.Decision{
		{Kind: "host-binding", Name: "old-chat-on-acme", ID: "b-1", DetachedAt: pruneDetachedAt, Decision: "prune"},
		{Kind: "policy", Name: "acme-tier-0", ID: "p-1", DetachedAt: pruneDetachedAt, Decision: "keep", Reason: "edited"},
	}}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(catalogprune.Decision{})); diff != "" {
		t.Fatalf("body (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"dry-run 0s"}, prune.calls); diff != "" {
		t.Fatalf("calls (-want +got):\n%s", diff)
	}
	em.Close()
	if evs := sink.all(); len(evs) != 0 {
		t.Fatalf("a dry run was audited: %+v", evs)
	}
}

// dryRun=false deletes, and each deleted row is audited as its own delete.
func TestCatalogPruneApplyAuditsEachDelete(t *testing.T) {
	deleted := prunedBinding
	deleted.Deleted = true
	prune := &fakePrune{rep: catalogprune.Report{Idle: 30 * 24 * time.Hour, Rows: []catalogprune.Decision{deleted, keptPolicy}, Pruned: 1, Kept: 1, Applied: true}}
	h, sink, em := newPruneHarness(t, prune)
	w := scopeReq(t, h, "token", http.MethodPost, "/catalog/prune?dryRun=false&idle=30d", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if diff := cmp.Diff([]string{"apply 720h0m0s"}, prune.calls); diff != "" {
		t.Fatalf("calls (-want +got):\n%s", diff)
	}
	em.Close()
	owner := meta.Owner{Kind: meta.OwnerSystem}
	wantEvents(t, sink.all(), audit.Event{
		Actor:    audit.Actor{Kind: audit.ActorAdminToken, Name: "admin-token"},
		Action:   "host-bindings.delete",
		Resource: audit.Resource{Kind: "host-binding", ID: "b-1", Name: "old-chat-on-acme", Owner: &owner},
		Outcome:  audit.Outcome{Status: audit.StatusAllowed, Code: http.StatusOK},
		Request:  audit.Request{Method: http.MethodPost, Path: "/catalog/prune"},
		Change:   &audit.Change{Fields: []string{audit.AnyField}},
	})
}

// A non-admin is refused before anything is judged.
func TestCatalogPruneRequiresAdmin(t *testing.T) {
	prune := &fakePrune{}
	h, sink, em := newPruneHarness(t, prune)
	w := scopeReq(t, h, "alice", http.MethodPost, "/catalog/prune?dryRun=false", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if len(prune.calls) != 0 {
		t.Fatalf("a non-admin reached the prune: %v", prune.calls)
	}
	em.Close()
	if evs := sink.all(); len(evs) != 1 || evs[0].Action != "system.catalog-prune" || evs[0].Outcome.Status != audit.StatusDenied {
		t.Fatalf("audit events = %+v, want one denied system.catalog-prune", evs)
	}
}

func TestCatalogPruneStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prune  *fakePrune
		query  string
		status int
	}{
		{"not wired", nil, "", http.StatusServiceUnavailable},
		{"idle that is not a duration", &fakePrune{}, "?idle=monthly", http.StatusBadRequest},
		{"idle of zero", &fakePrune{}, "?idle=0s", http.StatusBadRequest},
		{"another relay seeding", &fakePrune{err: appcatalog.ErrSeedBusy}, "?dryRun=false", http.StatusServiceUnavailable},
		{"prune failed", &fakePrune{err: errors.New("list models: connection reset")}, "?dryRun=false", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newPruneHarness(t, tc.prune)
			if w := scopeReq(t, h, "root", http.MethodPost, "/catalog/prune"+tc.query, ""); w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == http.StatusBadRequest && len(tc.prune.calls) != 0 {
				t.Fatalf("a bad request reached the prune: %v", tc.prune.calls)
			}
		})
	}
}
