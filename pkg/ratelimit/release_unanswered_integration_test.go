//go:build integration

package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestReleaseUnansweredRedis(t *testing.T) {
	runReleaseUnansweredSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

// A batch that never got a connection did not run its reserve, so no release follows it.
func TestReleaseUnansweredRedisSkipsUnsentBatch(t *testing.T) {
	cfg := kvtest.Config(t)
	rec := kvtest.NewRecording(kvtest.Connect(t, cfg))
	s := kvtest.ExhaustedPool(t, cfg)
	l := New(s, discardLog(), nil).ReleaseUnansweredOn(New(rec, discardLog(), nil))

	b := kv.NewBatch(s, 1)
	p, err := l.PrepareReserve(b, "unanswered-unsent", []Rule{conRule("unanswered:unsent:con", "concurrency", 1, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	b.Exec(context.Background())
	if _, err := p.Finish(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reserve: err = %v, want ErrUnavailable", err)
	}
	l.releases.Wait()
	if n := len(rec.Named("limit.commit")); n != 0 {
		t.Fatalf("release scripts = %d, want 0", n)
	}
}
