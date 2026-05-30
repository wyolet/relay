package clickhouse

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

var errFlush = errors.New("flush boom")

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildWhere(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		q         payload.Query
		wantParts []string
		wantArgs  int
	}{
		{
			name:      "empty",
			q:         payload.Query{},
			wantParts: nil,
			wantArgs:  0,
		},
		{
			name:      "since only is interpolated not bound",
			q:         payload.Query{Since: time.Hour},
			wantParts: []string{"ts >= now() - INTERVAL 3600 SECOND"},
			wantArgs:  0,
		},
		{
			name:      "from/to bound",
			q:         payload.Query{From: now.Add(-time.Hour), To: now},
			wantParts: []string{"ts >= ?", "ts <= ?"},
			wantArgs:  2,
		},
		{
			name:      "cursor tuple",
			q:         payload.Query{CursorTS: now, CursorID: "r1"},
			wantParts: []string{"(ts, request_id) < (?, ?)"},
			wantArgs:  2,
		},
		{
			name:      "multi-value IN + status range",
			q:         payload.Query{PolicyID: []string{"p1", "p2"}, ModelID: []string{"m1"}, StatusMin: 400, StatusMax: 599},
			wantParts: []string{"policy_id IN (?,?)", "model_id IN (?)", "status >= ?", "status <= ?"},
			wantArgs:  5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := buildWhere(tc.q)
			for _, p := range tc.wantParts {
				if !strings.Contains(where, p) {
					t.Errorf("where %q missing %q", where, p)
				}
			}
			if len(tc.wantParts) == 0 && where != "" {
				t.Errorf("expected empty where, got %q", where)
			}
			if len(args) != tc.wantArgs {
				t.Errorf("args: got %d want %d", len(args), tc.wantArgs)
			}
		})
	}
}
