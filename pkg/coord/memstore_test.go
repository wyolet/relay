package coord_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/coord"
)

var errStoreDown = errors.New("store unreachable")

// memStore is an in-memory coord.Store with switches for failure and hung publishes.
type memStore struct {
	mu       sync.Mutex
	ints     map[string]int64
	vals     map[string][]byte
	zsets    map[string]map[string]int64
	subs     map[string]map[int]func([]byte)
	nextSub  int
	events   []string // "subscribe:<channel>" and "pipeline" in call order
	batches  [][]coord.Cmd
	failing  bool
	pubErr   error
	pubBlock chan struct{} // non-nil: Publish waits for it to close or for ctx
}

func newMemStore() *memStore {
	return &memStore{
		ints:  map[string]int64{},
		vals:  map[string][]byte{},
		zsets: map[string]map[string]int64{},
		subs:  map[string]map[int]func([]byte){},
	}
}

func (s *memStore) Pipeline(_ context.Context, cmds []coord.Cmd) error {
	s.mu.Lock()
	s.events = append(s.events, "pipeline")
	if s.failing {
		s.mu.Unlock()
		return errStoreDown
	}
	s.batches = append(s.batches, slices.Clone(cmds))
	replies := make([]coord.Reply, len(cmds))
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

func (s *memStore) exec(c coord.Cmd) coord.Reply {
	switch c.Op {
	case coord.OpIncrBy:
		s.ints[c.Key] += c.Args[0]
		return coord.Reply{Int: s.ints[c.Key]}
	case coord.OpGet:
		return coord.Reply{Bytes: s.vals[c.Key]}
	case coord.OpSet:
		s.vals[c.Key] = slices.Clone(c.Value)
		return coord.Reply{}
	case coord.OpPExpire:
		return coord.Reply{}
	case coord.OpZAdd:
		z := s.zsets[c.Key]
		if z == nil {
			z = map[string]int64{}
			s.zsets[c.Key] = z
		}
		z[string(c.Value)] = c.Args[0]
		return coord.Reply{}
	case coord.OpZCount:
		n := int64(0)
		for _, sc := range s.zsets[c.Key] {
			if sc >= c.Args[0] && sc <= c.Args[1] {
				n++
			}
		}
		return coord.Reply{Int: n}
	case coord.OpZRangeByScore:
		type m struct {
			k  string
			sc int64
		}
		var ms []m
		for k, sc := range s.zsets[c.Key] {
			if sc >= c.Args[0] && sc <= c.Args[1] {
				ms = append(ms, m{k, sc})
			}
		}
		slices.SortFunc(ms, func(a, b m) int {
			if a.sc != b.sc {
				return int(a.sc - b.sc)
			}
			if a.k < b.k {
				return -1
			}
			return 1
		})
		r := coord.Reply{}
		for _, x := range ms {
			r.List = append(r.List, []byte(x.k))
			r.Int = max(r.Int, x.sc)
		}
		return r
	case coord.OpZRemRangeByScore:
		for k, sc := range s.zsets[c.Key] {
			if sc >= c.Args[0] && sc <= c.Args[1] {
				delete(s.zsets[c.Key], k)
			}
		}
		return coord.Reply{}
	}
	return coord.Reply{Err: errors.New("unsupported op")}
}

func (s *memStore) Publish(ctx context.Context, channel string, payload []byte) error {
	s.mu.Lock()
	block, perr := s.pubBlock, s.pubErr
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if perr != nil {
		return perr
	}
	s.mu.Lock()
	fns := make([]func([]byte), 0, len(s.subs[channel]))
	for _, fn := range s.subs[channel] {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(slices.Clone(payload))
	}
	return nil
}

func (s *memStore) Subscribe(ctx context.Context, channel string, fn func([]byte)) (func(), error) {
	s.mu.Lock()
	s.events = append(s.events, "subscribe:"+channel)
	id := s.nextSub
	s.nextSub++
	if s.subs[channel] == nil {
		s.subs[channel] = map[int]func([]byte){}
	}
	s.subs[channel][id] = fn
	s.mu.Unlock()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs[channel], id)
			s.mu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}

func (s *memStore) zadd(key, member string, score int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec(coord.Cmd{Op: coord.OpZAdd, Key: key, Args: [2]int64{score}, Value: []byte(member)})
}

func (s *memStore) setFailing(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = v
}

func (s *memStore) subscribers(channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[channel])
}

func (s *memStore) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func (s *memStore) sentBatches() [][]coord.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches)
}

func (s *memStore) intVal(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ints[key]
}

func waitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
