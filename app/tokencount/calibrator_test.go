package tokencount

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// runCalibratorSuite is the backend-independent contract, run against kv.Mem here and against a real Redis in the integration build.
func runCalibratorSuite(t *testing.T, s kv.Store) {
	t.Helper()

	t.Run("observe then read the session ratio", func(t *testing.T) {
		c := NewCalibrator(s)
		ctx := t.Context()
		c.Observe(ctx, "sess-a", "model-a", 4000, 1000)

		got, ok := c.Ratio(ctx, "sess-a", "model-a")
		if !ok {
			t.Fatal("ratio missing after Observe")
		}
		if got != 0.25 {
			t.Fatalf("ratio = %v, want 0.25", got)
		}
	})

	t.Run("session wins over model", func(t *testing.T) {
		c := NewCalibrator(s)
		ctx := t.Context()
		c.Observe(ctx, "", "model-b", 4000, 1000)      // model only: 0.25
		c.Observe(ctx, "sess-b", "model-c", 4000, 500) // session: 0.125

		got, ok := c.Ratio(ctx, "sess-b", "model-b")
		if !ok || got != 0.125 {
			t.Fatalf("ratio = %v (ok=%v), want the session's 0.125", got, ok)
		}
	})

	t.Run("falls back to the model ratio", func(t *testing.T) {
		c := NewCalibrator(s)
		ctx := t.Context()
		c.Observe(ctx, "", "model-d", 2000, 1000)

		got, ok := c.Ratio(ctx, "unknown-session", "model-d")
		if !ok || got != 0.5 {
			t.Fatalf("ratio = %v (ok=%v), want the model's 0.5", got, ok)
		}
	})

	t.Run("unknown session and model", func(t *testing.T) {
		c := NewCalibrator(s)
		if _, ok := c.Ratio(t.Context(), "nope", "nope"); ok {
			t.Fatal("ratio reported known for a pair never observed")
		}
	})

	t.Run("last observation wins", func(t *testing.T) {
		c := NewCalibrator(s)
		ctx := t.Context()
		c.Observe(ctx, "sess-e", "model-e", 4000, 1000)
		c.Observe(ctx, "sess-e", "model-e", 4000, 2000)

		got, _ := c.Ratio(ctx, "sess-e", "model-e")
		if got != 0.5 {
			t.Fatalf("ratio = %v, want the latest 0.5", got)
		}
	})

	t.Run("nothing measurable is not recorded", func(t *testing.T) {
		c := NewCalibrator(s)
		ctx := t.Context()
		c.Observe(ctx, "sess-f", "model-f", 0, 1000)
		c.Observe(ctx, "sess-f", "model-f", 4000, 0)

		if _, ok := c.Ratio(ctx, "sess-f", "model-f"); ok {
			t.Fatal("a zero body or zero token count must not be stored as a ratio")
		}
	})

	t.Run("a steady ratio is written once", func(t *testing.T) {
		cs := &countingStore{store: s}
		c := &Calibrator{state: cs}
		ctx := t.Context()
		c.Observe(ctx, "", "model-g", 4000, 1000)
		c.Observe(ctx, "", "model-g", 4000, 1020) // 2% move

		if n := cs.count(modelKey("model-g")); n != 1 {
			t.Fatalf("writes = %d, want 1", n)
		}
		if got, _ := c.Ratio(ctx, "", "model-g"); got != 0.25 {
			t.Fatalf("ratio = %v, want the first write's 0.25", got)
		}
	})

	t.Run("a move past the threshold is written", func(t *testing.T) {
		cs := &countingStore{store: s}
		c := &Calibrator{state: cs}
		ctx := t.Context()
		c.Observe(ctx, "", "model-h", 4000, 1000)
		c.Observe(ctx, "", "model-h", 4000, 1100) // 10% move

		if n := cs.count(modelKey("model-h")); n != 2 {
			t.Fatalf("writes = %d, want 2", n)
		}
		if got, _ := c.Ratio(ctx, "", "model-h"); got != 0.275 {
			t.Fatalf("ratio = %v, want 0.275", got)
		}
	})

	t.Run("a slow drift is measured from the last write", func(t *testing.T) {
		cs := &countingStore{store: s}
		c := &Calibrator{state: cs}
		ctx := t.Context()
		c.Observe(ctx, "", "model-i", 4000, 1000)
		c.Observe(ctx, "", "model-i", 4000, 1030) // 3% from the write: skipped
		c.Observe(ctx, "", "model-i", 4000, 1060) // 3% from the last sample, 6% from the write

		if n := cs.count(modelKey("model-i")); n != 2 {
			t.Fatalf("writes = %d, want 2", n)
		}
		if got, _ := c.Ratio(ctx, "", "model-i"); got != 0.265 {
			t.Fatalf("ratio = %v, want 0.265", got)
		}
	})

	t.Run("an aged write is refreshed", func(t *testing.T) {
		cs := &countingStore{store: s}
		now := time.Now()
		c := &Calibrator{state: cs, now: func() time.Time { return now }}
		ctx := t.Context()
		c.Observe(ctx, "sess-j", "model-j", 4000, 1000)
		now = now.Add(rewriteInterval - time.Second)
		c.Observe(ctx, "sess-j", "model-j", 4000, 1000)
		if n := cs.count(sessionKey("sess-j")); n != 1 {
			t.Fatalf("session writes before the interval = %d, want 1", n)
		}
		now = now.Add(time.Second)
		c.Observe(ctx, "sess-j", "model-j", 4000, 1000)

		if n := cs.count(sessionKey("sess-j")); n != 2 {
			t.Errorf("session writes = %d, want 2", n)
		}
		if n := cs.count(modelKey("model-j")); n != 2 {
			t.Errorf("model writes = %d, want 2", n)
		}
	})

	t.Run("concurrent samples of one key write once", func(t *testing.T) {
		cs := &countingStore{store: s}
		c := &Calibrator{state: cs}
		ctx := t.Context()
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() { c.Observe(ctx, "", "model-k", 4000, 1000) })
		}
		wg.Wait()

		if n := cs.count(modelKey("model-k")); n != 1 {
			t.Fatalf("writes = %d, want 1", n)
		}
	})
}

// countingStore counts writes per key and passes everything through to store.
type countingStore struct {
	store
	mu   sync.Mutex
	sets map[string]int
}

func (c *countingStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	if c.sets == nil {
		c.sets = map[string]int{}
	}
	c.sets[key]++
	c.mu.Unlock()
	return c.store.Set(ctx, key, value, ttl)
}

func (c *countingStore) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sets[key]
}

// Session ids accrue without limit, so the record of written ratios must not.
func TestCalibrator_WrittenRecordIsBounded(t *testing.T) {
	now := time.Now()
	c := &Calibrator{state: newRecordingStore(), now: func() time.Time { return now }}
	ctx := t.Context()

	for i := range maxWrittenEntries + 100 {
		c.Observe(ctx, "s"+strconv.Itoa(i), "", 4000, 1000)
	}
	if n := len(c.written); n > maxWrittenEntries {
		t.Fatalf("record holds %d entries, want at most %d", n, maxWrittenEntries)
	}

	// Once every entry is past the refresh interval, a new key evicts them all rather than an arbitrary share.
	for i := len(c.written); i < maxWrittenEntries; i++ {
		c.Observe(ctx, "fill"+strconv.Itoa(i), "", 4000, 1000)
	}
	now = now.Add(rewriteInterval)
	c.Observe(ctx, "fresh", "", 4000, 1000)
	if n := len(c.written); n != 1 {
		t.Fatalf("record holds %d entries after the stale ones expired, want 1", n)
	}
}

func TestCalibrator_Mem(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	runCalibratorSuite(t, s)
}

// recordingStore captures what was written with which TTL.
type recordingStore struct {
	mu   sync.Mutex
	sets map[string]time.Duration
}

func newRecordingStore() *recordingStore {
	return &recordingStore{sets: map[string]time.Duration{}}
}

func (r *recordingStore) Get(context.Context, string) ([]byte, error) { return nil, kv.ErrNotFound }

func (r *recordingStore) Set(_ context.Context, key string, _ []byte, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets[key] = ttl
	return nil
}

// Every key carries a TTL: a ratio is an observation about current traffic, and a stale one must lapse rather than keep answering.
func TestCalibrator_WritesBothKeysWithTTLs(t *testing.T) {
	rec := newRecordingStore()
	c := &Calibrator{state: rec}
	c.Observe(t.Context(), "sess-1", "model-1", 4000, 1000)

	if got := rec.sets[sessionKey("sess-1")]; got != sessionTTL {
		t.Errorf("session ttl = %v, want %v", got, sessionTTL)
	}
	if got := rec.sets[modelKey("model-1")]; got != modelTTL {
		t.Errorf("model ttl = %v, want %v", got, modelTTL)
	}
	if len(rec.sets) != 2 {
		t.Errorf("keys written = %v, want exactly the session and model keys", rec.sets)
	}
}

// A session id arrives from a request header, so an id that would reshape the key namespace is dropped — the model ratio is still recorded.
func TestCalibrator_RejectsUnusableSessionID(t *testing.T) {
	rec := newRecordingStore()
	c := &Calibrator{state: rec}
	c.Observe(t.Context(), "a:{b}", "model-2", 4000, 1000)

	if _, ok := rec.sets[sessionKey("a:{b}")]; ok {
		t.Error("an id carrying key syntax must not be written")
	}
	if len(rec.sets) != 1 {
		t.Errorf("keys written = %v, want the model key alone", rec.sets)
	}
}

func TestCalibrator_NilIsInert(t *testing.T) {
	var c *Calibrator
	c.Observe(t.Context(), "s", "m", 100, 10)
	if _, ok := c.Ratio(t.Context(), "s", "m"); ok {
		t.Fatal("a nil calibrator must report nothing known")
	}
}
