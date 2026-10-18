// Package usagetest is the conformance suite every usage.Reader backend runs: one fixed event fixture, one table of queries per Reader method, and the results every backend must return for them.
package usagetest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Open returns a Reader whose store holds exactly events, registering any teardown on t. Run calls it once and only reads from the result.
type Open func(t *testing.T, events []usage.Event) usage.Reader

// Run loads Fixture through open and checks Events, Count, Summary (alone and against a previous window) and TimeSeries against the expected results in this package's case tables.
func Run(t *testing.T, open Open) {
	t.Helper()
	r := open(t, Fixture())
	t.Run("events", func(t *testing.T) { runEvents(t, r) })
	t.Run("count", func(t *testing.T) { runCount(t, r) })
	t.Run("summary", func(t *testing.T) { runSummary(t, r, summaryCases) })
	t.Run("summary with previous", func(t *testing.T) { runSummaryWithPrevious(t, r) })
	t.Run("timeseries", func(t *testing.T) { runTimeSeries(t, r, timeSeriesCases) })
	t.Run("summary distinct", func(t *testing.T) { runSummary(t, r, summaryDistinctCases) })
	t.Run("timeseries distinct", func(t *testing.T) { runTimeSeries(t, r, timeSeriesDistinctCases) })
}

func flat(ms int64) usage.DurationStats {
	return usage.DurationStats{Avg: ms, P50: ms, P95: ms, P99: ms, Max: ms}
}

// mixed is a latency set over unequal samples; it leaves percentiles out because backends compute them differently there.
func mixed(avg, max int64) usage.DurationStats {
	return usage.DurationStats{Avg: avg, Max: max}
}

func ptr(s usage.DurationStats) *usage.DurationStats { return &s }

func normalizeStats(s usage.DurationStats, percentiles bool) usage.DurationStats {
	if !percentiles {
		s.P50, s.P95, s.P99 = 0, 0, 0
	}
	return s
}

func normalizeTTFT(s *usage.DurationStats, percentiles bool) *usage.DurationStats {
	if s == nil {
		return nil
	}
	n := normalizeStats(*s, percentiles)
	return &n
}

// normalizeTokens treats nil and empty maps alike: backends differ only in which one a row without tokens carries.
func normalizeTokens(m map[string]int64) map[string]int64 {
	if len(m) == 0 {
		return nil
	}
	return m
}

// normalizeTime makes instants comparable whatever location a backend returns them in.
func normalizeTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// sortByGroup orders summary rows by their group, because backends may return rows with equal request counts in any order.
func sortByGroup[T any](rows []T, group func(T) map[string]string) {
	key := func(i int) string { return fmt.Sprint(group(rows[i])) }
	sort.SliceStable(rows, func(i, j int) bool { return key(i) < key(j) })
}

func assertEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	t.Errorf("%s mismatch\n got: %s\nwant: %s", what, g, w)
}
