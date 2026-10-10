//go:build integration

package otlpreceiver_test

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/app/otlpreceiver"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestMarkers_Redis(t *testing.T) {
	cfg := kvtest.Config(t)
	s := kvtest.Connect(t, cfg)
	runMarkersSuite(t, s)

	t.Run("markers expire within a day", func(t *testing.T) {
		c := call(11, 1)
		if _, err := otlpreceiver.NewMarkers(s).Mark(t.Context(), tenant, otlpreceiver.MarkerUsage, []otlpreceiver.Call{c}); err != nil {
			t.Fatalf("Mark: %v", err)
		}
		raw := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
		t.Cleanup(func() { _ = raw.Close() })
		ttl, err := raw.PTTL(t.Context(), "{otlp:"+tenant+":"+c.TraceID+"}:usage:"+c.SpanID).Result()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		if ttl <= 23*time.Hour || ttl > 24*time.Hour {
			t.Fatalf("marker ttl = %v, want just under 24h", ttl)
		}
	})
}
