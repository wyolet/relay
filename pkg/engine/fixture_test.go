package engine_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

type team struct {
	ID, Name string
	Ver      int64
	Limit    int
}

type user struct {
	ID, Name, TeamID string
	Ver              int64
}

type userPlan struct {
	User *user
	Team *team
}

type teamPlan struct {
	Team  *team
	Users []*user
}

// src is an in-memory Source.
type src[T any] struct {
	mu   sync.Mutex
	rows map[string]*T
	id   func(*T) string
	gets atomic.Int32
	err  error
}

func newSrc[T any](id func(*T) string, rows ...*T) *src[T] {
	s := &src[T]{rows: map[string]*T{}, id: id}
	for _, r := range rows {
		s.rows[id(r)] = r
	}
	return s
}

func (s *src[T]) List(context.Context) ([]*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return slices.Collect(maps.Values(s.rows)), nil
}

func (s *src[T]) Get(_ context.Context, id string) (*T, error) {
	s.gets.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.rows[id], nil
}

func (s *src[T]) put(r *T) {
	s.mu.Lock()
	s.rows[s.id(r)] = r
	s.mu.Unlock()
}

func (s *src[T]) del(id string) {
	s.mu.Lock()
	delete(s.rows, id)
	s.mu.Unlock()
}

func teamID(t *team) string { return t.ID }
func userID(u *user) string { return u.ID }

type fixture struct {
	t     *testing.T
	e     *engine.Engine
	teams *src[team]
	users *src[user]

	mu        sync.Mutex
	calls     map[string]int
	stats     []engine.PublishStats
	errs      []error
	published chan engine.Generation
}

func (f *fixture) count(name string) {
	f.mu.Lock()
	f.calls[name]++
	f.mu.Unlock()
}

func (f *fixture) callsOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fixture) resetCalls() {
	f.mu.Lock()
	clear(f.calls)
	f.mu.Unlock()
}

// newFixture registers teams and users as inputs, a team plan grouping users
// by team (Watch), and a user plan reading its team by id. Register more
// controllers with extra before Load.
func newFixture(t *testing.T, o engine.Options, extra func(f *fixture)) *fixture {
	t.Helper()
	f := &fixture{
		t:         t,
		teams:     newSrc(teamID, &team{ID: "t1", Name: "alpha", Limit: 10}, &team{ID: "t2", Name: "beta", Limit: 20}),
		users:     newSrc(userID, &user{ID: "u1", Name: "ann", TeamID: "t1"}, &user{ID: "u2", Name: "bob", TeamID: "t1"}, &user{ID: "u3", Name: "cy", TeamID: "t2"}),
		calls:     map[string]int{},
		published: make(chan engine.Generation, 64),
	}
	o.Hooks.OnPublish = func(v *engine.View, s engine.PublishStats) {
		f.mu.Lock()
		f.stats = append(f.stats, s)
		f.mu.Unlock()
		select {
		case f.published <- v.Gen():
		default:
		}
	}
	o.Hooks.OnReconcileError = func(_ string, _ engine.Ref, err error) {
		f.mu.Lock()
		f.errs = append(f.errs, err)
		f.mu.Unlock()
	}
	f.e = engine.New(o)
	engine.RegisterKind(f.e, engine.KindSpec[team]{Kind: "team", Source: f.teams, Meta: func(t *team) engine.Meta {
		return engine.Meta{ID: t.ID, Name: t.Name, Version: t.Ver}
	}})
	engine.RegisterKind(f.e, engine.KindSpec[user]{Kind: "user", Source: f.users, Shards: 4, Meta: func(u *user) engine.Meta {
		return engine.Meta{ID: u.ID, Name: u.Name, Version: u.Ver}
	}})
	engine.RegisterKind(f.e, engine.KindSpec[teamPlan]{Kind: "teamplan"})
	engine.RegisterKind(f.e, engine.KindSpec[userPlan]{Kind: "userplan", Shards: 8})

	tc := engine.NewController("team-plan", "teamplan", func(tx *engine.Txn, id string) error {
		f.count("team-plan/" + id)
		tm := engine.Get[team](tx, id)
		if tm == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &teamPlan{Team: tm, Users: engine.Members[user](tx)})
		return nil
	})
	engine.Watch(tc, func(t *team) []string { return []string{t.ID} })
	engine.Watch(tc, func(u *user) []string { return []string{u.TeamID} })
	f.e.Register(tc)

	uc := engine.NewController("user-plan", "userplan", func(tx *engine.Txn, id string) error {
		f.count("user-plan/" + id)
		u := engine.Get[user](tx, id)
		if u == nil {
			engine.Drop(tx)
			return nil
		}
		tm := engine.Get[team](tx, u.TeamID)
		if tm == nil {
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: "ref_missing", Target: engine.Ref{Kind: "team", ID: u.TeamID}, Field: "teamId"})
			engine.Drop(tx)
			return nil
		}
		if prev := engine.Get[userPlan](tx, id); prev != nil && prev.User == u && *prev.Team == *tm {
			engine.Put(tx, prev)
			return nil
		}
		engine.Put(tx, &userPlan{User: u, Team: tm})
		return nil
	})
	engine.Watch(uc, func(u *user) []string { return []string{u.ID} })
	f.e.Register(uc)

	if extra != nil {
		extra(f)
	}
	if err := f.e.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = f.e.Close() })
	return f
}

func (f *fixture) view() *engine.View { return f.e.View() }

func (f *fixture) lastStats() engine.PublishStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats[len(f.stats)-1]
}

// waitGen blocks until generation gen or later is published.
func (f *fixture) waitGen(gen engine.Generation) {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for f.e.View().Gen() < gen {
		select {
		case <-f.published:
		case <-deadline:
			f.t.Fatalf("generation %d not published (at %d)", gen, f.e.View().Gen())
		}
	}
}

func mustApply[T any](t *testing.T, e *engine.Engine, row *T) {
	t.Helper()
	if err := engine.Apply(e, row); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func codes(is []engine.Issue) []string {
	out := make([]string, len(is))
	for i, x := range is {
		out[i] = x.Code
	}
	return out
}

var errBoom = errors.New("boom")
