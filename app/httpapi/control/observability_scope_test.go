package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/usagelog"
)

type fakeKeyLister []*key.Key

func (f fakeKeyLister) List(context.Context) ([]*key.Key, error) { return f, nil }

func testKeys() fakeKeyLister {
	mk := func(hash string, owner meta.Owner) *key.Key {
		k := &key.Key{}
		k.Meta = meta.Metadata{ID: meta.NewID(), Name: "k-" + hash, Owner: owner}
		k.Spec.KeyHash = hash
		return k
	}
	return fakeKeyLister{
		mk("hash-alice-1", meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}),
		mk("hash-alice-2", meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}),
		mk("hash-bob", meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}),
		mk("hash-operator", meta.Owner{Kind: meta.OwnerUser}),
	}
}

func TestScopeOf(t *testing.T) {
	keys := testKeys()

	tests := []struct {
		name         string
		authzr       authz.Authorizer
		who          *actor.Actor
		wantHashes   []string
		unrestricted bool
	}{
		{"single-user authorizer is unrestricted", authz.AlwaysAllowAuthenticated{}, scopeActors["alice"], nil, true},
		{"admin role is unrestricted", testRBAC(), scopeActors["root"], nil, true},
		{"admin token is unrestricted", testRBAC(), scopeActors["token"], nil, true},
		{"user is scoped to own hashes", testRBAC(), scopeActors["alice"], []string{"hash-alice-1", "hash-alice-2"}, false},
		{"user with no keys is scoped to nothing", testRBAC(), &actor.Actor{UserID: "u-carol"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := actor.WithActor(context.Background(), tt.who)
			sc, err := scopeOf(ctx, tt.authzr, nil, keys, "usage")
			if err != nil {
				t.Fatal(err)
			}
			if sc.unrestricted != tt.unrestricted {
				t.Fatalf("unrestricted = %v, want %v", sc.unrestricted, tt.unrestricted)
			}
			if len(sc.hashes) != len(tt.wantHashes) {
				t.Fatalf("hashes = %v, want %v", sc.hashes, tt.wantHashes)
			}
			for i := range sc.hashes {
				if sc.hashes[i] != tt.wantHashes[i] {
					t.Fatalf("hashes = %v, want %v", sc.hashes, tt.wantHashes)
				}
			}
		})
	}

	t.Run("nil lister fails closed", func(t *testing.T) {
		ctx := actor.WithActor(context.Background(), scopeActors["alice"])
		sc, err := scopeOf(ctx, testRBAC(), nil, nil, "usage")
		if err != nil || sc.unrestricted || len(sc.hashes) != 0 || len(sc.projectIDs) != 0 {
			t.Fatalf("got (%+v, %v), want scoped-to-nothing", sc, err)
		}
	})
}

func TestScopeEventQuery(t *testing.T) {
	t.Run("unrestricted leaves the query alone", func(t *testing.T) {
		q := usagelog.EventQuery{}
		if !scopeEventQuery(&q, readScope{unrestricted: true}) {
			t.Fatal("want true")
		}
		if len(q.ScopeProjectID) != 0 || len(q.ScopeRelayKeyHash) != 0 {
			t.Fatalf("query narrowed: %+v", q)
		}
	})
	t.Run("scope rides the query as a disjunction", func(t *testing.T) {
		q := usagelog.EventQuery{}
		sc := readScope{projectIDs: []string{"p-1"}, hashes: []string{"h-1", "h-2"}}
		if !scopeEventQuery(&q, sc) {
			t.Fatal("want true")
		}
		if len(q.ScopeProjectID) != 1 || len(q.ScopeRelayKeyHash) != 2 {
			t.Fatalf("query = %+v, want the scope carried verbatim", q)
		}
	})
	t.Run("a caller filter is not widened by the scope", func(t *testing.T) {
		q := usagelog.EventQuery{RelayKeyHash: []string{"h-foreign"}}
		if !scopeEventQuery(&q, readScope{hashes: []string{"h-1"}}) {
			t.Fatal("want true")
		}
		// The caller's own filter stays; the scope is ANDed on top of it.
		if len(q.RelayKeyHash) != 1 || q.RelayKeyHash[0] != "h-foreign" {
			t.Fatalf("RelayKeyHash = %v, want the caller filter untouched", q.RelayKeyHash)
		}
		if len(q.ScopeRelayKeyHash) != 1 {
			t.Fatalf("ScopeRelayKeyHash = %v", q.ScopeRelayKeyHash)
		}
	})
	t.Run("an empty scope matches nothing", func(t *testing.T) {
		q := usagelog.EventQuery{}
		if scopeEventQuery(&q, readScope{}) {
			t.Fatal("want false")
		}
	})
}

// fakeUsageReader records the query it was handed; calls counts invocations
// so tests can assert the scoped-out short-circuit never reaches the store.
type fakeUsageReader struct {
	calls  int
	last   usagelog.EventQuery
	events []usagelog.Event
}

func (f *fakeUsageReader) Events(_ context.Context, q usagelog.EventQuery) ([]usagelog.Event, error) {
	f.calls++
	f.last = q
	return f.events, nil
}

func (f *fakeUsageReader) Summary(_ context.Context, q usagelog.SummaryQuery) (usagelog.SummaryResult, error) {
	f.calls++
	f.last = q.EventQuery
	return usagelog.SummaryResult{Rows: []usagelog.SummaryRow{}}, nil
}

func (f *fakeUsageReader) TimeSeries(_ context.Context, q usagelog.TimeSeriesQuery) (usagelog.TimeSeriesResult, error) {
	f.calls++
	f.last = q.EventQuery
	return usagelog.TimeSeriesResult{Rows: []usagelog.TimeSeriesRow{}}, nil
}

// newUsageHarness mounts the usage + logs read endpoints behind the actor-
// injecting middleware from scope_test.go. Stores is nil, so a scoped
// caller resolves to zero owned keys (the fail-closed path).
func newUsageHarness(t *testing.T, authzr authz.Authorizer, reader *fakeUsageReader) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("usage-scope-test", "0"))
	d := Deps{Authz: authzr, UsageReader: reader}
	registerUsage(api, d, nil)
	registerLogs(api, d, nil)
	return r
}

func TestUsageReadScoping(t *testing.T) {
	t.Run("scoped user with no keys gets an empty page without touching the store", func(t *testing.T) {
		reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1"}}}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/events", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		var out struct {
			Events []usagelog.Event `json:"events"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Events) != 0 {
			t.Fatalf("events = %v, want none", out.Events)
		}
		if reader.calls != 0 {
			t.Fatalf("reader called %d times, want 0", reader.calls)
		}
	})

	t.Run("admin reads unscoped", func(t *testing.T) {
		reader := &fakeUsageReader{}
		h := newUsageHarness(t, testRBAC(), reader)
		w := scopeReq(t, h, "root", http.MethodGet, "/usage/events", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if reader.calls != 1 || len(reader.last.RelayKeyHash) != 0 {
			t.Fatalf("calls=%d filter=%v, want one unfiltered call", reader.calls, reader.last.RelayKeyHash)
		}
	})

	t.Run("single-user authorizer reads unscoped", func(t *testing.T) {
		reader := &fakeUsageReader{}
		h := newUsageHarness(t, authz.AlwaysAllowAuthenticated{}, reader)
		w := scopeReq(t, h, "alice", http.MethodGet, "/usage/summary", "")
		if w.Code != 200 {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if reader.calls != 1 || len(reader.last.RelayKeyHash) != 0 {
			t.Fatalf("calls=%d filter=%v, want one unfiltered call", reader.calls, reader.last.RelayKeyHash)
		}
	})

	t.Run("scoped user cannot fetch a foreign log record", func(t *testing.T) {
		reader := &fakeUsageReader{events: []usagelog.Event{{RequestID: "r-1", RelayKeyHash: "hash-bob"}}}
		h := newUsageHarness(t, testRBAC(), reader)
		if w := scopeReq(t, h, "alice", http.MethodGet, "/logs/r-1", ""); w.Code != 404 {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body)
		}
		if w := scopeReq(t, h, "root", http.MethodGet, "/logs/r-1", ""); w.Code != 200 {
			t.Fatalf("admin status = %d, want 200: %s", w.Code, w.Body)
		}
	})
}
