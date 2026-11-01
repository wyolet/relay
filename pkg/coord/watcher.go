package coord

import (
	"context"
	"math"
	"time"
)

// pushBuffer bounds pushes waiting for the Run goroutine; one dropped here is
// recovered by the next watermark re-list.
const pushBuffer = 256

// WatchOptions configures one Watcher.
type WatchOptions struct {
	Key       string // the sorted set of member → version, hash-tagged
	Topic     string // pub/sub channel carrying member ids; empty = poll only
	Every     time.Duration
	FullEvery time.Duration // 0 = never after the first list
	Apply     func(members [][]byte)
	Full      func(members [][]byte) // nil = Apply
}

// Watcher is list-then-watch over one changed-set: it lists the whole set
// once, applies pushes at once, re-lists the members above the highest
// version it has seen every Every, and the whole set every FullEvery. Apply
// and Full run on the Run goroutine only.
type Watcher struct {
	s      Store
	n      *Notifier
	o      WatchOptions
	pushes chan []byte
	mark   int64 // highest version seen by a list
	listed bool  // a full list has succeeded
}

// NewWatcher returns a watcher; n may be nil for poll-only.
func NewWatcher(s Store, n *Notifier, o WatchOptions) *Watcher {
	if o.Every <= 0 || o.Apply == nil {
		panic("coord: WatchOptions needs Every and Apply")
	}
	if o.Full == nil {
		o.Full = o.Apply
	}
	return &Watcher{s: s, n: n, o: o, pushes: make(chan []byte, pushBuffer)}
}

// Run watches until ctx ends. It subscribes before the first list, so a
// change between the two is seen at least once.
func (w *Watcher) Run(ctx context.Context) error {
	subscribed := w.subscribe(ctx)
	every := time.NewTicker(w.o.Every)
	defer every.Stop()
	var full <-chan time.Time
	if w.o.FullEvery > 0 {
		ft := time.NewTicker(w.o.FullEvery)
		defer ft.Stop()
		full = ft.C
	}
	w.list(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-w.pushes:
			w.o.Apply(w.drainPushes(m))
		case <-every.C:
			if !subscribed {
				subscribed = w.subscribe(ctx)
			}
			w.list(ctx, !w.listed)
		case <-full:
			w.list(ctx, true)
		}
	}
}

func (w *Watcher) subscribe(ctx context.Context) bool {
	if w.n == nil || w.o.Topic == "" {
		return true
	}
	return w.n.Subscribe(ctx, w.o.Topic, w.push) == nil
}

func (w *Watcher) push(member []byte) {
	select {
	case w.pushes <- member:
	default:
	}
}

// drainPushes batches the pushes already waiting into one Apply.
func (w *Watcher) drainPushes(first []byte) [][]byte {
	out := [][]byte{first}
	for {
		select {
		case m := <-w.pushes:
			out = append(out, m)
		default:
			return out
		}
	}
}

// list reads the whole set (full) or the members above the watermark and
// hands them over; a failed list changes nothing and is retried next period.
func (w *Watcher) list(ctx context.Context, full bool) {
	lo := int64(math.MinInt64)
	if !full {
		lo = w.mark + 1
	}
	var members [][]byte
	var top int64
	var rerr error
	cmd := Cmd{Op: OpZRangeByScore, Key: w.o.Key, Args: [2]int64{lo, math.MaxInt64}, Reply: func(r Reply) {
		members, top, rerr = r.List, r.Int, r.Err
	}}
	lctx, cancel := context.WithTimeout(ctx, w.o.Every)
	err := w.s.Pipeline(lctx, []Cmd{cmd})
	cancel()
	if err != nil || rerr != nil {
		return
	}
	if len(members) > 0 {
		w.mark = max(w.mark, top)
	}
	if full {
		w.listed = true
		w.o.Full(members)
	} else if len(members) > 0 {
		w.o.Apply(members)
	}
}
