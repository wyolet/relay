package usage

import "sort"

// ttftUs is the event's upstream time-to-first-byte in µs. ok is false when the event never reached upstream (no timing to aggregate).
func ttftUs(ev Event) (int64, bool) {
	if ev.Upstream == nil {
		return 0, false
	}
	return ev.Upstream.ResponseStart, true
}

// ttftStats aggregates µs samples and reports ms, nil when there are none. Averaging before converting matches the SQL backends (avg of µs / 1000, truncated) rather than averaging already-truncated milliseconds.
func ttftStats(samplesUs []int64) *DurationStats {
	if len(samplesUs) == 0 {
		return nil
	}
	s := durationStats(samplesUs)
	return &DurationStats{Avg: s.Avg / 1000, P50: s.P50 / 1000, P95: s.P95 / 1000, P99: s.P99 / 1000, Max: s.Max / 1000}
}

// durationStats computes avg + percentiles + max from raw samples. For
// dogfood-sized inputs (thousands of values per group) a full sort is
// fine. Production scale would use t-digest or HDR histogram.
func durationStats(samples []int64) DurationStats {
	if len(samples) == 0 {
		return DurationStats{}
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return DurationStats{
		Avg: sum / int64(len(sorted)),
		P50: percentile(sorted, 0.50),
		P95: percentile(sorted, 0.95),
		P99: percentile(sorted, 0.99),
		Max: sorted[len(sorted)-1],
	}
}

func percentile(sortedAsc []int64, p float64) int64 {
	if len(sortedAsc) == 0 {
		return 0
	}
	idx := int(float64(len(sortedAsc)-1) * p)
	return sortedAsc[idx]
}
