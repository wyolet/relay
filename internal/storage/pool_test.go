package storage

import "testing"

func TestResolvePoolSettings(t *testing.T) {
	tests := []struct {
		name             string
		opts             []PoolOption
		wantMax, wantMin int32
	}{
		{"defaults when no opts", nil, 10, 2},
		{"non-positive ignored (keeps defaults)", []PoolOption{WithMaxConns(0), WithMinConns(-1)}, 10, 2},
		{"overrides applied", []PoolOption{WithMaxConns(20), WithMinConns(10)}, 20, 10},
		{"min clamps to max", []PoolOption{WithMaxConns(5), WithMinConns(50)}, 5, 5},
		{"only max set, min stays default", []PoolOption{WithMaxConns(30)}, 30, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePoolSettings(tt.opts...)
			if got.maxConns != tt.wantMax || got.minConns != tt.wantMin {
				t.Errorf("resolvePoolSettings() = max %d/min %d, want max %d/min %d",
					got.maxConns, got.minConns, tt.wantMax, tt.wantMin)
			}
		})
	}
}
