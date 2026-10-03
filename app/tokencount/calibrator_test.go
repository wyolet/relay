package tokencount

import (
	"context"
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
