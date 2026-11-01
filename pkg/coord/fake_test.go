package coord

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"sync"
)

var errDown = errors.New("store down")

// fakeStore is an in-memory Store for tests: counters, values, sorted sets
// and synchronous pub/sub.
type fakeStore struct {
	mu        sync.Mutex
	ints      map[string]int64
	vals      map[string][]byte
	zsets     map[string]map[string]int64
	ttls      map[string]int64
	subs      map[string]map[int]func([]byte)
	nextSub   int
	down      bool
	pipelines int
	sent      [][]Cmd
	gate      chan struct{} // when set, Publish waits on it
	published []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		ints:  map[string]int64{},
		vals:  map[string][]byte{},
		zsets: map[string]map[string]int64{},
		ttls:  map[string]int64{},
		subs:  map[string]map[int]func([]byte){},
	}
}

func (s *fakeStore) setDown(d bool) {
	s.mu.Lock()
	s.down = d
	s.mu.Unlock()
}

func (s *fakeStore) Pipeline(_ context.Context, cmds []Cmd) error {
	s.mu.Lock()
	if s.down {
		s.mu.Unlock()
		return errDown
	}
	s.pipelines++
	s.sent = append(s.sent, slices.Clone(cmds))
	replies := make([]Reply, len(cmds))
	for i, c := range cmds {
		replies[i] = s.exec(c)
	}
	s.mu.Unlock()
	for i, c := range cmds {
		if c.Reply != nil {
			c.Reply(replies[i])
		}
	}
	return nil
}

func (s *fakeStore) exec(c Cmd) Reply {
	z := s.zsets[c.Key]
	lo, hi := c.Args[0], c.Args[1]
	switch c.Op {
	case OpIncrBy:
		s.ints[c.Key] += c.Args[0]
		return Reply{Int: s.ints[c.Key]}
	case OpGet:
		if v, ok := s.ints[c.Key]; ok {
			return Reply{Int: v, Bytes: []byte(strconv.FormatInt(v, 10))}
		}
		return Reply{Bytes: s.vals[c.Key]}
	case OpSet:
		s.vals[c.Key] = slices.Clone(c.Value)
		s.ttls[c.Key] = c.Args[1]
	case OpPExpire:
		s.ttls[c.Key] = c.Args[1]
	case OpZAdd:
		if z == nil {
			z = map[string]int64{}
			s.zsets[c.Key] = z
		}
		z[string(c.Value)] = c.Args[0]
	case OpZCount:
		var n int64
		for _, sc := range z {
			if sc >= lo && sc <= hi {
				n++
			}
		}
		return Reply{Int: n}
	case OpZRangeByScore:
		type pair struct {
			m  string
			sc int64
		}
		var ps []pair
		for m, sc := range z {
			if sc >= lo && sc <= hi {
				ps = append(ps, pair{m, sc})
			}
		}
		slices.SortFunc(ps, func(a, b pair) int { return cmp.Or(cmp.Compare(a.sc, b.sc), cmp.Compare(a.m, b.m)) })
		var r Reply
		for _, p := range ps {
			r.List = append(r.List, []byte(p.m))
			r.Int = p.sc
		}
		return r
	case OpZRemRangeByScore:
		for m, sc := range z {
			if sc >= lo && sc <= hi {
				delete(z, m)
			}
		}
	}
	return Reply{}
}

func (s *fakeStore) Publish(ctx context.Context, channel string, payload []byte) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	if s.down {
		s.mu.Unlock()
		return errDown
	}
	s.published = append(s.published, string(payload))
	fns := slices.Collect(maps.Values(s.subs[channel]))
	s.mu.Unlock()
	for _, fn := range fns {
		fn(payload)
	}
	return nil
}

func (s *fakeStore) Subscribe(_ context.Context, channel string, fn func([]byte)) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errDown
	}
	if s.subs[channel] == nil {
		s.subs[channel] = map[int]func([]byte){}
	}
	id := s.nextSub
	s.nextSub++
	s.subs[channel][id] = fn
	return func() {
		s.mu.Lock()
		delete(s.subs[channel], id)
		s.mu.Unlock()
	}, nil
}

func (s *fakeStore) zadd(key, member string, score int64) {
	s.mu.Lock()
	if s.zsets[key] == nil {
		s.zsets[key] = map[string]int64{}
	}
	s.zsets[key][member] = score
	s.mu.Unlock()
}

func (s *fakeStore) counter(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ints[key]
}
