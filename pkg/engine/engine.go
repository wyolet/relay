package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/engine/cell"
)

// Engine owns the object maps, the graph, the controllers and the publish
// loop. One per process.
type Engine struct {
	opts Options
	sch  *schema
	view atomic.Pointer[View]

	mu        sync.Mutex // the write side: everything below until qmu
	st        state
	retries   map[Ref]time.Time // failed outputs and when to retry them
	fetchWake time.Time
	resyncDue bool
	stopped   bool

	qmu   sync.Mutex
	queue map[Ref]Event
	wake  chan struct{}

	hooks hookQueue

	sealed   atomic.Bool // registration closed
	loaded   atomic.Bool
	closed   atomic.Bool
	closing  chan struct{}
	loopDone chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// state is the working copy the drain goroutine edits; publish freezes it.
type state struct {
	objs   []shardWriter[string, obj]
	names  []shardWriter[string, []string]
	graph  graph
	issues issueStore
	outs   map[Ref]*outState
	links  map[cell.Key]int // live link count per cell
}

// outState is what the engine remembers about one output between reconciles.
type outState struct {
	edges    []Edge
	links    []cell.Key
	failures int
}

// Options holds the knobs the composition root sets.
type Options struct {
	// Debounce is how long Notify events collect before one drain. 0 drains
	// on every event.
	Debounce time.Duration
	// MaxRounds bounds the fixpoint loop per drained batch (default 8); past
	// it the remaining dirty outputs are abandoned and a Resync scheduled.
	MaxRounds int
	Hooks     Hooks
	Cells     cell.Linker // the cell registry; nil disables Txn.Link
}

// Hooks are the observation points; every field is optional, so a caller sets one without implementing the rest.
//
// Hooks run after the engine lock is released, one call at a time and in the order their events happened (OnPublish in generation order), so a hook may call Apply, Delete or Resync; it must not call Close.
type Hooks struct {
	OnPublish        func(v *View, s PublishStats)
	OnReconcileError func(controller string, out Ref, err error)
	OnBatchAbandoned func(gen Generation, rounds int)
	OnResync         func(changed int, dur time.Duration)
}

// PublishStats describes one batch, so a metrics hook has numbers without
// walking the view.
type PublishStats struct {
	Gen        Generation
	Inputs     int // input rows applied
	Dirtied    int // derived objects marked dirty
	Reconciled int // reconcile calls made
	Rounds     int
	Duration   time.Duration
}

const (
	minRetry   = 100 * time.Millisecond // retry base when Debounce is 0
	maxBackoff = time.Minute
)

var (
	errNotLoaded = errors.New("engine: not loaded")
	errClosed    = errors.New("engine: closed")
)

// New constructs an engine; RegisterKind and Register run before Load.
func New(o Options) *Engine {
	if o.MaxRounds <= 0 {
		o.MaxRounds = 8
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		opts:     o,
		sch:      newSchema(),
		retries:  map[Ref]time.Time{},
		queue:    map[Ref]Event{},
		wake:     make(chan struct{}, 1),
		closing:  make(chan struct{}),
		loopDone: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	e.hooks.init()
	e.view.Store(emptyView(e.sch))
	return e
}

func (e *Engine) mustBeUnloaded(what string) {
	if e.sealed.Load() {
		panic("engine: " + what + " after Load")
	}
}

// Load lists every input kind through its Source, runs every controller to fixpoint and publishes generation 1. Boot and nothing else; the hooks for generation 1 have run when it returns.
func (e *Engine) Load(ctx context.Context) error {
	err := e.load(ctx)
	e.hooks.flush()
	return err
}

func (e *Engine) load(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed.Load():
		return errClosed
	case e.loaded.Load():
		return errors.New("engine: Load called twice")
	}
	e.sealed.Store(true)
	e.sch.resolve()
	e.reset()
	b := e.newBatch()
	for _, k := range e.sch.kinds {
		if !k.input() {
			continue
		}
		rows, err := k.list(ctx)
		if err != nil {
			return fmt.Errorf("engine: list %s: %w", k.kind, err)
		}
		for _, row := range rows {
			id := k.meta(row).ID
			if id == "" {
				return fmt.Errorf("engine: %s row without an id", k.kind)
			}
			if b.write(k, id, row) {
				b.stats.Inputs++
			}
		}
	}
	b.settle()
	b.publish(true)
	e.loaded.Store(true)
	go e.loop()
	e.afterBatch(b)
	return nil
}

// reset empties the working state, so a failed Load can be retried.
func (e *Engine) reset() {
	n := len(e.sch.kinds)
	e.st = state{
		objs:   make([]shardWriter[string, obj], n),
		names:  make([]shardWriter[string, []string], n),
		graph:  newGraph(),
		issues: newIssueStore(),
		outs:   map[Ref]*outState{},
		links:  map[cell.Key]int{},
	}
	for i, k := range e.sch.kinds {
		e.st.objs[i].cur = newShardMap[string, obj](k.shards)
		e.st.names[i].cur = newShardMap[string, []string](k.shards)
	}
	for _, c := range e.sch.ctls {
		clear(c.dirty)
		for _, w := range c.watches {
			clear(w.byOut)
		}
	}
	clear(e.retries)
	e.view.Store(emptyView(e.sch))
}

func (e *Engine) objects(k *kindInfo) objects {
	return objects{e.st.objs[k.idx].cur, e.st.names[k.idx].cur}
}

// Notify queues an event. Never blocks: the queue coalesces by Ref, keeping
// the latest op and the highest version (0, which forces a fetch, wins).
func (e *Engine) Notify(ev Event) {
	if e.closed.Load() {
		return
	}
	e.qmu.Lock()
	if old, ok := e.queue[ev.Ref]; ok {
		if old.Version == 0 || ev.Version == 0 {
			ev.Version = 0
		} else {
			ev.Version = max(old.Version, ev.Version)
		}
	}
	e.queue[ev.Ref] = ev
	e.qmu.Unlock()
	e.kick()
}

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// View returns the current published view; cheap and lock-free.
func (e *Engine) View() *View { return e.view.Load() }

// Close drains the queue once and stops. No publish and no hook call after it returns.
func (e *Engine) Close() error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	e.mu.Lock()
	started := e.loaded.Load()
	e.mu.Unlock()
	if started {
		close(e.closing)
		<-e.loopDone
	}
	e.mu.Lock()
	if started {
		e.drainLocked(context.Background())
	}
	e.stopped = true
	e.cancel()
	e.mu.Unlock()
	e.hooks.flush()
	return nil
}
