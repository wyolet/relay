package inference

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/model"
)

func TestSnapshotCreated(t *testing.T) {
	cases := []struct {
		name     string
		s        model.Snapshot
		fallback int64
		want     int64
	}{
		{"empty released falls back", model.Snapshot{}, 999, 999},
		{"malformed released falls back", model.Snapshot{ReleasedAt: "yesterday"}, 999, 999},
		{"valid date parsed", model.Snapshot{ReleasedAt: "2024-11-20"}, 999, time.Date(2024, 11, 20, 0, 0, 0, 0, time.UTC).Unix()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotCreated(&tc.s, tc.fallback); got != tc.want {
				t.Errorf("snapshotCreated = %d, want %d", got, tc.want)
			}
		})
	}
}
