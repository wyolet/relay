package coord_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/coord"
)

// adder adds one INCRBY on its key per beat and records the ticks and replies it saw.
type adder struct {
	key     string
	mu      sync.Mutex
	ticks   []coord.Tick
	replies []int64
}

func (p *adder) Collect(t coord.Tick, b *coord.Batch) {
	p.mu.Lock()
	p.ticks = append(p.ticks, t)
	p.mu.Unlock()
	b.Add(coord.Cmd{Op: coord.OpIncrBy, Key: p.key, Args: [2]int64{1}, Reply: func(r coord.Reply) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.replies = append(p.replies, r.Int)
	}})
}

func (p *adder) seen() ([]coord.Tick, []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ticks), slices.Clone(p.replies)
}

// startTicker runs tk until the test ends or the returned close is called.
func startTicker(t *testing.T, tk *coord.Ticker) (closeTicker func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tk.Run(context.Background())
	}()
	closed := false
	closeTicker = func() {
		if closed {
			return
		}
		closed = true
		if err := tk.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return after Close")
		}
	}
	t.Cleanup(closeTicker)
	return closeTicker
}

func TestTickerSendsEveryParticipantInOnePipelinePerBeat(t *testing.T) {
	s := newMemStore()
	tk := coord.NewTicker(s, coord.TickerOptions{MinPeriod: 10 * time.Millisecond})
	tk.Register(&adder{key: "k1"})
	tk.Register(&adder{key: "k2"})
	startTicker(t, tk)

	waitUntil(t, "three beats", func() bool { return len(s.sentBatches()) >= 3 })
	for _, b := range s.sentBatches() {
		keys := make([]string, 0, len(b))
		for _, c := range b {
			keys = append(keys, c.Key)
		}
		if !slices.Equal(keys, []string{"k1", "k2"}) {
			t.Fatalf("beat sent %v, want both participants' commands in one pipeline", keys)
		}
	}
}

func TestTickNumbersIncreaseByOneAndRepliesArrive(t *testing.T) {
	s := newMemStore()
	tk := coord.NewTicker(s, coord.TickerOptions{MinPeriod: 10 * time.Millisecond})
	p := &adder{key: "k"}
	tk.Register(p)
	startTicker(t, tk)

	waitUntil(t, "five beats", func() bool { ticks, _ := p.seen(); return len(ticks) >= 5 })
	ticks, replies := p.seen()
	for i := 1; i < len(ticks); i++ {
		if ticks[i].N != ticks[i-1].N+1 {
			t.Fatalf("tick numbers %v do not increase by one", ticks)
		}
	}
	for i, r := range replies {
		if r != int64(i+1) {
			t.Fatalf("replies %v, want the running INCRBY totals 1, 2, 3, ...", replies)
		}
	}
}

func TestCloseSendsFinalBeatWithZeroN(t *testing.T) {
	s := newMemStore()
	tk := coord.NewTicker(s, coord.TickerOptions{MinPeriod: 10 * time.Millisecond})
	p := &adder{key: "k"}
	tk.Register(p)
	closeTicker := startTicker(t, tk)

	waitUntil(t, "two beats", func() bool { ticks, _ := p.seen(); return len(ticks) >= 2 })
	closeTicker()
	ticks, _ := p.seen()
	if last := ticks[len(ticks)-1]; last.N != 0 {
		t.Fatalf("last tick N = %d, want 0 for the final beat", last.N)
	}
	if got := s.intVal("k"); got != int64(len(ticks)) {
		t.Fatalf("store holds %d of %d collected increments", got, len(ticks))
	}
	time.Sleep(30 * time.Millisecond)
	if after, _ := p.seen(); len(after) != len(ticks) {
		t.Fatalf("participant collected %d more times after Close", len(after)-len(ticks))
	}
}

func TestOutageAfterGraceThenRecovery(t *testing.T) {
	s := newMemStore()
	outage := make(chan time.Time, 4)
	recovered := make(chan time.Duration, 4)
	tk := coord.NewTicker(s, coord.TickerOptions{
		MinPeriod: 10 * time.Millisecond,
		Grace:     50 * time.Millisecond,
		Hooks: coord.TickerHooks{
			OnOutage:  func(since time.Time) { outage <- since },
			OnRecover: func(down time.Duration) { recovered <- down },
		},
	})
	tk.Register(&adder{key: "k"})
	startTicker(t, tk)
	waitUntil(t, "a good beat", func() bool { return !tk.Status().LastGood.IsZero() })
	if tk.Status().Outage {
		t.Fatal("Outage set while the store answers")
	}

	s.setFailing(true)
	var since time.Time
	select {
	case since = <-outage:
	case <-time.After(3 * time.Second):
		t.Fatal("OnOutage not called")
	}
	st := tk.Status()
	if !st.Outage || !st.LastGood.Equal(since) {
		t.Fatalf("Status during outage = %+v, OnOutage since = %v; want Outage with LastGood = since", st, since)
	}

	s.setFailing(false)
	select {
	case down := <-recovered:
		if down < 50*time.Millisecond {
			t.Fatalf("OnRecover down = %v, want at least the grace", down)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnRecover not called")
	}
	if st := tk.Status(); st.Outage || !st.LastGood.After(since) {
		t.Fatalf("Status after recovery = %+v, want no outage and a newer LastGood", st)
	}
}

func TestFailureShorterThanGraceIsNotAnOutage(t *testing.T) {
	s := newMemStore()
	outage := make(chan time.Time, 4)
	tk := coord.NewTicker(s, coord.TickerOptions{
		MinPeriod: 10 * time.Millisecond,
		Grace:     time.Second,
		Hooks:     coord.TickerHooks{OnOutage: func(since time.Time) { outage <- since }},
	})
	tk.Register(&adder{key: "k"})
	startTicker(t, tk)
	waitUntil(t, "a good beat", func() bool { return !tk.Status().LastGood.IsZero() })

	s.setFailing(true)
	time.Sleep(100 * time.Millisecond)
	if tk.Status().Outage {
		t.Fatal("Outage set within the grace")
	}
	s.setFailing(false)
	select {
	case <-outage:
		t.Fatal("OnOutage called for a failure shorter than the grace")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPresenceCountsLiveProcesses(t *testing.T) {
	s := newMemStore()
	opts := func(pod string) coord.TickerOptions {
		return coord.TickerOptions{MinPeriod: 10 * time.Millisecond, PodID: pod, PresenceKey: "{pods}"}
	}
	a := coord.NewTicker(s, opts("pod-a"))
	b := coord.NewTicker(s, opts("pod-b"))
	startTicker(t, a)
	startTicker(t, b)
	waitUntil(t, "both processes to see two live pods", func() bool {
		return a.Status().LivePods == 2 && b.Status().LivePods == 2
	})
}

func TestLivePodsIsOneWithoutPresence(t *testing.T) {
	s := newMemStore()
	tk := coord.NewTicker(s, coord.TickerOptions{MinPeriod: 10 * time.Millisecond})
	tk.Register(&adder{key: "k"})
	startTicker(t, tk)
	waitUntil(t, "a good beat", func() bool { return !tk.Status().LastGood.IsZero() })
	if n := tk.Status().LivePods; n != 1 {
		t.Fatalf("LivePods = %d without presence, want 1", n)
	}
}
