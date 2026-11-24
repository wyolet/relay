package control

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"context"
	"slices"
	"sync"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
)

func TestCursor(t *testing.T) {
	ts := time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)
	const id = "req_01HXYZ:weird-but-valid"
	gotTS, gotID, err := decodeCursor(encodeCursor(ts, id))
	if err != nil || !gotTS.Equal(ts) || gotID != id {
		t.Fatalf("round trip = %v, %q, %v; want %v, %q to the nanosecond", gotTS, gotID, err, ts, id)
	}
	// Not base64, no separator, and a timestamp that is not a number.
	for _, bad := range []string{"not base64!!", "", "bm9jb2xvbg", "eDp5"} {
		if _, _, err := decodeCursor(bad); statusOf(t, err) != 400 || err.Error() != "invalid cursor" {
			t.Errorf("decodeCursor(%q) = %v, want a 400 invalid cursor", bad, err)
		}
	}
}

func TestEffectiveLimit(t *testing.T) {
	for in, want := range map[int]int{-3: 100, 0: 100, 1: 1, 50: 50, 10000: 10000, 10001: 10000, 1 << 30: 10000} {
		if got := effectiveLimit(in); got != want {
			t.Errorf("effectiveLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseTagFilters(t *testing.T) {
	got, err := parseTagFilters([]string{"session_id:s1", "session_id:s2", "leg:translate", "colon:a:b", "empty:"})
	want := map[string][]string{"session_id": {"s1", "s2"}, "leg": {"translate"}, "colon": {"a:b"}, "empty": {""}}
	if diff := cmp.Diff(want, got); err != nil || diff != "" {
		t.Fatalf("parse: %v (-want +got):\n%s", err, diff)
	}
	if got, err := parseTagFilters(nil); err != nil || got != nil {
		t.Fatalf("no tags = %+v, %v; want no filter", got, err)
	}
	for _, bad := range []string{"nocolon", ":valueonly", ""} {
		if _, err := parseTagFilters([]string{"ok:1", bad}); statusOf(t, err) != 400 || err.Error() != "invalid `tag` value (want key:value): "+bad {
			t.Errorf("parseTagFilters(%q) = %v, want a 400 naming the value", bad, err)
		}
	}
}

func TestParseSince(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"": time.Hour, "  ": time.Hour, "90m": 90 * time.Minute, " 2h ": 2 * time.Hour, "7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, "0s": 0,
	} {
		if got, err := parseSince(raw); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "d", "1dd", "7", "1w"} {
		if _, err := parseSince(bad); statusOf(t, err) != 400 || err.Error() != "invalid `since` value: "+bad {
			t.Errorf("parseSince(%q) = %v, want a 400 naming the value", bad, err)
		}
	}
	for _, bug := range []struct{ raw, wrong string }{
		{"1h1d", "48h: the d suffix rescales the whole value, not its own term; want 25h or a 400"},
		{"1000000d", "-1620477h52m48.5s: the day conversion overflows silently; want a 400"},
		{"-1h", "-1h: a window ending before it starts is accepted; want a 400 as for to before from"},
	} {
		t.Run(bug.raw, func(t *testing.T) {
			if got, err := parseSince(bug.raw); err == nil && got != 25*time.Hour {
				t.Fatalf("parseSince(%q) = %v, want 25h or an error", bug.raw, got)
			}
		})
	}
}

// A query the handler cannot read is the caller's 400, answered before the store is asked anything.
func TestUsageQueryValidation(t *testing.T) {
	for path, want := range map[string]string{
		"/usage/events?since=soon":                                "invalid `since` value: soon",
		"/usage/events?from=yesterday":                            "invalid `from` (want RFC3339): yesterday",
		"/usage/summary?to=tomorrow":                              "invalid `to` (want RFC3339): tomorrow",
		"/logs?from=2026-03-02T00:00:00Z&to=2026-03-01T00:00:00Z": "`to` must not be before `from`",
		"/usage/events?status_class=3xx":                          "`status_class` must be 2xx, 4xx, or 5xx",
		"/usage/timeseries?interval=1h&tag=nocolon":               "invalid `tag` value (want key:value): nocolon",
		"/logs?cursor=!!":                                         "invalid cursor",
		"/usage/timeseries":                                       "`interval` is required (e.g. \"1h\", \"1d\")",
		"/usage/timeseries?interval=0s":                           "`interval` must be positive",
		"/usage/timeseries?interval=-1h":                          "`interval` must be positive",
		"/usage/timeseries?interval=soon":                         "invalid `since` value: soon",
		"/usage/timeseries?interval=1ns&since=1h":                 "interval too small for the requested window: would exceed the bucket cap — widen interval or shorten since",
		"/usage/summary?group_by=nonsense":                        "invalid group_by: nonsense",
		"/usage/timeseries?interval=1h&group_by=nonsense":         "invalid group_by: nonsense",
	} {
		t.Run(path, func(t *testing.T) {
			reader := &fakeUsageReader{}
			wantAPIError(t, scopeReq(t, newUsageHarness(t, testRBAC(), reader), "root", http.MethodGet, path, ""), 400, "", want)
			if reader.calls != 0 {
				t.Errorf("reached the store %d times", reader.calls)
			}
		})
	}
}

// A summary over a window with no events still reports the window it covered: a zero from/to gives a chart nothing to draw an empty range on.
func TestUsageSummaryReportsTheRequestedWindowWhenEmpty(t *testing.T) {
	h := newUsageHarness(t, testRBAC(), &fakeUsageReader{})
	before := time.Now()
	w := scopeReq(t, h, "root", http.MethodGet, "/usage/summary?since=2h", "")
	var out struct {
		From, To time.Time
		Rows     []json.RawMessage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("status = %d (%v): %s", w.Code, err, w.Body)
	}
	if out.To.Sub(out.From) != 2*time.Hour || out.To.Before(before) || out.To.After(time.Now()) || out.Rows == nil || len(out.Rows) != 0 {
		t.Fatalf("window = %v..%v with rows %v, want the 2h ending now and no rows", out.From, out.To, out.Rows)
	}
}

func TestUsageSummaryGroupByNoneReturnsOneRowWithAnEmptyGroup(t *testing.T) {
	now := time.Now().UTC()
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Source: "pipeline", Timestamp: now, Status: 200},
		{RequestID: "r-2", Source: "proxy", Timestamp: now, Status: 429},
		{RequestID: "r-3", Source: "proxy", Timestamp: now, Status: 503},
		{RequestID: "r-4", Source: "pipeline", Timestamp: now, ErrorKind: "no_keys"},
	}}
	h := newUsageHarness(t, testRBAC(), reader)
	w := scopeReq(t, h, "root", http.MethodGet, "/usage/summary?group_by=none", "")
	var out struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Rows) != 1 {
		t.Fatalf("status = %d (%v), want one row: %s", w.Code, err, w.Body)
	}
	for field, want := range map[string]string{
		"group": "{}", "requests": "3", "error_count": "2", "errors_4xx": "1", "errors_5xx": "1", "throttled": "1",
	} {
		if got := string(out.Rows[0][field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
}

// viewerFixture is a team with projects p1 and p2 and a viewer role bound at p1 alone.
func viewerFixture(t *testing.T) (cat *appcatalog.Catalog, viewer *actor.Actor, p1ID string) {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	yes := true
	var viewerRoleID string
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		if r.Meta.Name == "viewer" {
			viewerRoleID = r.Meta.ID
		}
	}
	teamID, p1, p2, viewerUserID := ids.New(), ids.New(), ids.New(), ids.New()
	tm := &team.Team{Meta: meta.Metadata{ID: teamID, Name: "t1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	mkProject := func(id, name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}}
		p.Spec.TeamID, p.Spec.Enabled = teamID, &yes
		return p
	}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "viewer-p1", Owner: meta.Owner{Kind: meta.OwnerProject, ID: p1}}}
	rb.Spec.RoleID = viewerRoleID
	rb.Spec.Scope = meta.Owner{Kind: meta.OwnerProject, ID: p1}
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: viewerUserID}}
	rb.Spec.Enabled = &yes

	cat = catalogtest.Catalog{
		Teams:        []*team.Team{tm},
		Projects:     []*project.Project{mkProject(p1, "p1"), mkProject(p2, "p2")},
		Roles:        builtins,
		RoleBindings: []*rolebinding.RoleBinding{rb},
	}.Load(t)
	return cat, &actor.Actor{UserID: viewerUserID, Subjects: appcatalog.UserSubjects(viewerUserID, nil, nil)}, p1
}

// A user reads the traffic of the service accounts they own, their bots, by principal; another user does not.
func TestScopeOf_UserServiceAccounts(t *testing.T) {
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "alice-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}}
	bot.StampOwner()
	cat := catalogtest.Catalog{ServiceAccounts: []*serviceaccount.ServiceAccount{bot}}.Load(t)
	botEvent := usagelog.Event{PrincipalKind: "serviceaccount", PrincipalID: bot.Meta.ID, LaneAccountID: ids.New()}
	for who, want := range map[string]readScope{
		"alice": {principalIDs: []string{"u-alice", bot.Meta.ID}},
		"bob":   {principalIDs: []string{"u-bob"}},
	} {
		got, err := scopeOf(actor.WithActor(context.Background(), scopeActors[who]), testRBAC(), cat, "usage")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got, cmp.AllowUnexported(readScope{}), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("scope of %s (-want +got):\n%s", who, diff)
		}
		if got.allows(botEvent) != (who == "alice") {
			t.Errorf("%s reads alice's bot's event: %v", who, got.allows(botEvent))
		}
	}
}

// A service account a team owns itself carries no project, so its traffic reads by principal: a reader at the team gets it, a reader at one of the team's projects does not.
func TestScopeOf_TeamServiceAccounts(t *testing.T) {
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	yes := true
	roleID := map[string]string{}
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
	}
	teamID, p1, teamReader, projectReader := ids.New(), ids.New(), ids.New(), ids.New()
	tm := &team.Team{Meta: meta.Metadata{ID: teamID, Name: "t1", Owner: meta.Global}, Spec: team.Spec{Enabled: &yes}}
	proj := &project.Project{Meta: meta.Metadata{ID: p1, Name: "p1"}, Spec: project.Spec{TeamID: teamID, Enabled: &yes}}
	proj.StampOwner()
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "t1-bot", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}}
	bot.StampOwner()
	bind := func(scope meta.Owner, roleName, userID string) *rolebinding.RoleBinding {
		b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: roleName + "-" + userID[len(userID)-6:]}}
		b.Spec.RoleID, b.Spec.Scope, b.Spec.Enabled = roleID[roleName], scope, &yes
		b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: userID}}
		b.StampOwner()
		return b
	}
	cat := catalogtest.Catalog{
		Teams: []*team.Team{tm}, Projects: []*project.Project{proj}, ServiceAccounts: []*serviceaccount.ServiceAccount{bot}, Roles: builtins,
		RoleBindings: []*rolebinding.RoleBinding{
			bind(meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, "viewer", teamReader),
			bind(meta.Owner{Kind: meta.OwnerProject, ID: p1}, "viewer", projectReader),
		},
	}.Load(t)
	bound := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	botEvent := usagelog.Event{TeamID: teamID, PrincipalID: bot.Meta.ID, RelayKeyHash: "hash-bot"}

	for _, tc := range []struct {
		who      string
		want     readScope
		readsBot bool
	}{
		{teamReader, readScope{projectIDs: []string{p1}, principalIDs: []string{bot.Meta.ID, teamReader}}, true},
		{projectReader, readScope{projectIDs: []string{p1}, principalIDs: []string{projectReader}}, false},
	} {
		who := &actor.Actor{UserID: tc.who, Subjects: appcatalog.UserSubjects(tc.who, nil, nil)}
		got, err := scopeOf(actor.WithActor(context.Background(), who), bound, cat, "usage")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(readScope{}), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("scope of %s (-want +got):\n%s", tc.who, diff)
		}
		if got.allows(botEvent) != tc.readsBot {
			t.Errorf("reader %s reads the team service account's event: %v, want %v", tc.who, got.allows(botEvent), tc.readsBot)
		}
	}
}

// A caller reads the whole stream only with a grant at the global scope. Otherwise the scope is the projects a binding lets them read, their own principal, and their own key hashes, so their traffic stays theirs across a key rotation and for events that carry a hash alone.
func TestScopeOf(t *testing.T) {
	cat, viewer, p1 := viewerFixture(t)
	bound := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	yes := true
	k := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "alice-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}}
	k.Spec.Enabled, k.Spec.KeyHash, k.Spec.PreviousKeyHash = &yes, "hash-alice-now", "hash-alice-old"
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: "u-alice"}
	withKey := catalogtest.Catalog{Keys: []*key.Key{k}}.Load(t)
	alice, whole := scopeActors["alice"], readScope{unrestricted: true}

	for _, tc := range []struct {
		name   string
		authzr authz.Authorizer
		cat    *appcatalog.Catalog
		who    *actor.Actor
		kind   string
		want   readScope
	}{
		{"single-user authorizer", authz.AlwaysAllowAuthenticated{}, nil, alice, "usage", whole},
		{"admin role", testRBAC(), nil, scopeActors["root"], "usage", whole},
		{"admin token", testRBAC(), nil, scopeActors["token"], "logs", whole},
		{"a user is scoped to their own principal", testRBAC(), nil, alice, "usage", readScope{principalIDs: []string{"u-alice"}}},
		{"no session identity is scoped to nothing", testRBAC(), cat, &actor.Actor{}, "usage", readScope{}},
		{"a project binding adds that project alone", bound, cat, viewer, "usage", readScope{projectIDs: []string{p1}, principalIDs: []string{viewer.UserID}}},
		// The viewer role grants usage and not logs: the two kinds resolve apart.
		{"a usage grant is not a logs grant", bound, cat, viewer, "logs", readScope{principalIDs: []string{viewer.UserID}}},
		{"own key hashes, current and previous", testRBAC(), withKey, alice, "usage",
			readScope{principalIDs: []string{"u-alice"}, keyHashes: []string{"hash-alice-now", "hash-alice-old"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scopeOf(actor.WithActor(context.Background(), tc.who), tc.authzr, tc.cat, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(got.keyHashes)
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(readScope{}), cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("scope (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadScopeAllows(t *testing.T) {
	sc := readScope{projectIDs: []string{"p-1"}, principalIDs: []string{"u-alice"}, keyHashes: []string{"hash-alice-old"}}
	for _, tc := range []struct {
		name string
		sc   readScope
		ev   usagelog.Event
		want bool
	}{
		{"own principal after a rotation", sc, usagelog.Event{RelayKeyHash: "hash-brand-new", PrincipalID: "u-alice"}, true},
		{"a readable project", sc, usagelog.Event{ProjectID: "p-1", PrincipalID: "u-bob"}, true},
		{"a hash-only event of an own key", sc, usagelog.Event{RelayKeyHash: "hash-alice-old"}, true},
		{"another principal's event", sc, usagelog.Event{RelayKeyHash: "hash-bob", PrincipalID: "u-bob", ProjectID: "p-2"}, false},
		{"an event naming nothing", sc, usagelog.Event{}, false},
		{"an empty scope", readScope{}, usagelog.Event{PrincipalID: "u-alice"}, false},
		{"an empty scope and an event naming nothing", readScope{}, usagelog.Event{}, false},
		{"the whole stream", readScope{unrestricted: true}, usagelog.Event{PrincipalID: "u-bob"}, true},
	} {
		if got := tc.sc.allows(tc.ev); got != tc.want {
			t.Errorf("%s: allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The scope rides the query beside the caller's own filters, never widening them; a scope that matches nothing must stop the handler, because an empty scope downstream means unfiltered.
func TestScopeEventQuery(t *testing.T) {
	filtered := usagelog.EventQuery{RelayKeyHash: []string{"h-foreign"}, ProjectID: []string{"p-9"}}
	scoped := filtered
	scoped.ScopeProjectID, scoped.ScopePrincipalID, scoped.ScopeRelayKeyHash = []string{"p-1"}, []string{"u-1", "u-2"}, []string{"h-1"}
	for _, tc := range []struct {
		name string
		sc   readScope
		ok   bool
		want usagelog.EventQuery
	}{
		{"the whole stream leaves the query alone", readScope{unrestricted: true}, true, filtered},
		{"a scope is carried verbatim", readScope{projectIDs: []string{"p-1"}, principalIDs: []string{"u-1", "u-2"}, keyHashes: []string{"h-1"}}, true, scoped},
		{"an empty scope matches nothing", readScope{}, false, filtered},
		{"key hashes alone are a scope", readScope{keyHashes: []string{"h-1"}}, true, func() usagelog.EventQuery {
			q := filtered
			q.ScopeRelayKeyHash = []string{"h-1"}
			return q
		}()},
	} {
		q := filtered
		if ok := scopeEventQuery(&q, tc.sc); ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
		}
		if diff := cmp.Diff(tc.want, q, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s: query (-want +got):\n%s", tc.name, diff)
		}
	}
}

// fakeUsageReader records the query it was handed; calls counts invocations
// so tests can assert the scoped-out short-circuit never reaches the store.
// The mutex covers /logs?count=true, which calls Events and Count concurrently, and /usage/facets, which runs one Facet per dimension concurrently.
type fakeUsageReader struct {
	mu         sync.Mutex
	calls      int
	last       usagelog.EventQuery
	lastCount  usagelog.EventQuery
	countLimit int
	distinct   string
	events     []usagelog.Event
}

func (f *fakeUsageReader) Events(_ context.Context, q usagelog.EventQuery) ([]usagelog.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = q
	return f.events, nil
}

func (f *fakeUsageReader) Count(_ context.Context, q usagelog.EventQuery, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastCount, f.countLimit = q, limit
	return usage.CountEvents(f.events, q, limit), nil
}

func (f *fakeUsageReader) Summary(_ context.Context, q usagelog.SummaryQuery) (usagelog.SummaryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last, f.distinct = q.EventQuery, q.Distinct
	return usage.SummarizeWithDistinct(f.events, q.GroupBy, q.Distinct, usage.CountsRefused(q.EventQuery, q.GroupBy))
}

func (f *fakeUsageReader) TimeSeries(_ context.Context, q usagelog.TimeSeriesQuery) (usagelog.TimeSeriesResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last, f.distinct = q.EventQuery, q.Distinct
	return usage.BucketizeWithDistinct(usage.FilterEvents(f.events, q.EventQuery), q.Interval, q.GroupBy, q.Distinct, usage.CountsRefused(q.EventQuery, q.GroupBy))
}

func (f *fakeUsageReader) Facet(_ context.Context, q usagelog.FacetQuery) ([]usagelog.FacetValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = q.EventQuery
	return usage.TopValues(usage.FilterEvents(f.events, q.EventQuery), q.Dim, q.Limit, usage.CountsRefused(q.EventQuery, q.Dim))
}

// newUsageHarness mounts the usage + logs read endpoints behind the actor-
// injecting middleware from scope_test.go. Stores is nil, so a scoped
// caller resolves to zero owned keys (the fail-closed path).
func newUsageHarness(t *testing.T, authzr authz.Authorizer, reader *fakeUsageReader) http.Handler {
	t.Helper()
	r, api := testAPI(t, scopeActors)
	d := Deps{Authz: authzr, UsageReader: reader}
	registerUsage(api, d, nil)
	registerUsageFacets(api, d, nil)
	registerLogs(api, d, nil)
	return r
}

// Every read route narrows to the caller's scope: a scoped caller's query carries it, a caller with no scope at all never reaches the store, and a caller who reads the whole stream sends none.
func TestUsageReadScoping(t *testing.T) {
	scopeOfQuery := func(q usagelog.EventQuery) [3][]string {
		return [3][]string{q.ScopeProjectID, q.ScopePrincipalID, q.ScopeRelayKeyHash}
	}
	for _, path := range []string{"/usage/events", "/usage/summary", "/usage/timeseries?interval=1h", "/logs", "/usage/facets?dims=model_id"} {
		for _, tc := range []struct {
			name, who string
			authzr    authz.Authorizer
			calls     int
			want      [3][]string
		}{
			{"a user reads their own principal", "alice", testRBAC(), 1, [3][]string{nil, {"u-alice"}, nil}},
			{"an admin reads unscoped", "root", testRBAC(), 1, [3][]string{}},
			{"the single-user authorizer reads unscoped", "alice", authz.AlwaysAllowAuthenticated{}, 1, [3][]string{}},
			{"no principal reads nothing", "nobody", testRBAC(), 0, [3][]string{}},
		} {
			reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1", Timestamp: time.Now()}}}
			w := scopeReq(t, newUsageHarness(t, tc.authzr, reader), tc.who, http.MethodGet, path, "")
			if w.Code != 200 || reader.calls != tc.calls {
				t.Fatalf("%s, %s: status %d after %d store calls, want 200 after %d: %s", path, tc.name, w.Code, reader.calls, tc.calls, w.Body)
			}
			if diff := cmp.Diff(tc.want, scopeOfQuery(reader.last), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s, %s: scope on the query (-want +got):\n%s", path, tc.name, diff)
			}
			if tc.calls == 0 && strings.Contains(w.Body.String(), "r-1") {
				t.Errorf("%s, %s: the body carries an event: %s", path, tc.name, w.Body)
			}
		}
	}
}

// A single record is read through the same scope: another principal's answers as absent.
func TestLogGetIsScoped(t *testing.T) {
	reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1", RelayKeyHash: "hash-bob", PrincipalID: "u-bob"}}}
	h := newUsageHarness(t, testRBAC(), reader)
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodGet, "/logs/r-1", ""), 404, "", "no log record for request id r-1")
	for _, who := range []string{"bob", "root"} {
		if code, out := getLog(t, h, who, "r-1"); code != 200 || out.Log.RequestID != "r-1" || out.Log.PrincipalID != "u-bob" {
			t.Errorf("%s: %d %+v, want bob's record", who, code, out.Log)
		}
	}
}

// Generated clients send list filters as repeated params; each repeat must reach the query, not just the first.
func TestUsageListFiltersTakeRepeatedParams(t *testing.T) {
	for _, path := range []string{"/usage/events", "/usage/summary", "/usage/timeseries?interval=1h", "/logs"} {
		reader := &fakeUsageReader{}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		w := scopeReq(t, newUsageHarness(t, testRBAC(), reader), "root", http.MethodGet, path+sep+"model_id=m1&model_id=m2&tag=env:prod&tag=team:a&status=429&status=503", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, w.Code, w.Body)
		}
		got := usagelog.EventQuery{ModelID: reader.last.ModelID, Status: reader.last.Status, Tags: reader.last.Tags}
		want := usagelog.EventQuery{ModelID: []string{"m1", "m2"}, Status: []int{429, 503}, Tags: map[string][]string{"env": {"prod"}, "team": {"a"}}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s: query (-want +got):\n%s", path, diff)
		}
	}
}

// dims repeats, lists, or both; the answer has exactly the dimensions asked for.
func TestUsageFacetsTakeRepeatedDims(t *testing.T) {
	h := newFacetsHarness(t, &fakeUsageReader{}, nil)
	for query, want := range map[string][]string{
		"dims=model_id&dims=host_id":         {"host_id", "model_id"},
		"dims=model_id,host_id":              {"host_id", "model_id"},
		"dims=model_id&dims=host_id,team_id": {"host_id", "model_id", "team_id"},
	} {
		code, out, body := getFacets(t, h, "root", query)
		if got := slices.Sorted(maps.Keys(out.Facets)); code != http.StatusOK || !slices.Equal(got, want) {
			t.Errorf("%s: %d with dims %v, want %v: %s", query, code, got, want, body)
		}
	}
}
