package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The slow-op WARN is logged at most once per interval, and the next line carries every op held back since the previous one.
func TestSlowOpLogSampled(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	slowLastLogNanos.Store(0)
	slowSinceLog.Store(0)

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })
	r := &Redis{client: client}

	now := time.Now()
	for range 5 {
		r.trackSlow("get", "{t}:k", now, context.DeadlineExceeded)
	}
	r.trackSlow("get", "{t}:k", now, nil) // fast and fine: neither counted nor logged
	if n := strings.Count(buf.String(), "slow or failed redis ops"); n != 1 {
		t.Fatalf("logged %d lines within one interval, want 1:\n%s", n, buf.String())
	}

	buf.Reset()
	slowLastLogNanos.Store(time.Now().Add(-2 * slowLogInterval).UnixNano())
	r.trackSlow("set", "{t}:k", now, context.DeadlineExceeded)
	var line struct {
		Ops int64
		Op  string
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v (%q)", err, buf.String())
	}
	if line.Ops != 5 || line.Op != "set" {
		t.Fatalf("line = %+v, want ops 5 (4 held back + this one), op set", line)
	}
}
