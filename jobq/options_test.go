package jobq

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestOptionsWithDefaults(t *testing.T) {
	got := Options{}.withDefaults()
	if got.Concurrency != 8 || got.PollInterval != time.Second || got.ScheduleInterval != time.Second ||
		got.RescueInterval != 30*time.Second || got.RescueAfter != 15*time.Minute || got.JobTimeout != 5*time.Minute ||
		got.Backoff == nil || got.Logger != slog.Default() {
		t.Errorf("defaults = %+v", got)
	}
	got = Options{Concurrency: -1, PollInterval: -1, ScheduleInterval: -1, RescueInterval: -1, RescueAfter: -1, JobTimeout: -1}.withDefaults()
	if got.Concurrency != 8 || got.PollInterval != time.Second || got.ScheduleInterval != time.Second ||
		got.RescueInterval != 30*time.Second || got.RescueAfter != 15*time.Minute || got.JobTimeout != 5*time.Minute {
		t.Errorf("negative settings = %+v, want the defaults", got)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	set := Options{
		Concurrency: 2, PollInterval: 3, ScheduleInterval: 4, RescueInterval: 5, RescueAfter: 7, JobTimeout: 6,
		Backoff: func(attempt int) time.Duration { return time.Duration(attempt) }, Logger: logger,
	}
	got = set.withDefaults()
	if got.Concurrency != 2 || got.PollInterval != 3 || got.ScheduleInterval != 4 || got.RescueInterval != 5 ||
		got.RescueAfter != 7 || got.JobTimeout != 6 || got.Backoff(9) != 9 || got.Logger != logger {
		t.Errorf("configured settings were replaced: %+v", got)
	}
}

// A job rescued while its handler may still run would execute twice.
func TestOptionsWithDefaults_PanicsUnlessRescueAfterExceedsJobTimeout(t *testing.T) {
	for name, o := range map[string]Options{
		"equal":                             {JobTimeout: time.Minute, RescueAfter: time.Minute},
		"shorter":                           {JobTimeout: 2 * time.Minute, RescueAfter: time.Minute},
		"default rescue, long job timeout":  {JobTimeout: 15 * time.Minute},
		"default job timeout, short rescue": {RescueAfter: 5 * time.Minute},
	} {
		func() {
			defer func() {
				if r := recover(); r != "jobq: RescueAfter must exceed JobTimeout" {
					t.Errorf("%s: recovered %v", name, r)
				}
			}()
			o.withDefaults()
		}()
	}
}

func TestDefaultBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		base    time.Duration
	}{
		{-5, time.Second}, {0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second},
		{12, 2048 * time.Second}, {13, time.Hour}, {14, time.Hour}, {1 << 30, time.Hour},
	} {
		for range 200 {
			// Up to a fifth of jitter on top of the doubled delay.
			if got := defaultBackoff(tc.attempt); got < tc.base || got > tc.base+tc.base/5 {
				t.Fatalf("defaultBackoff(%d) = %v, want within [%v, %v]", tc.attempt, got, tc.base, tc.base+tc.base/5)
			}
		}
	}
}
