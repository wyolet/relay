package usage

import "sort"

// ttftMs derives upstream time-to-first-byte in ms. ok is false when the
// event never reached upstream (no timing to aggregate).
func ttftMs(ev Event) (int64, bool) {
	if ev.Upstream == nil {
		return 0, false
	}
	return ev.Upstream.ResponseStart / 1000, true // µs → ms
}

// optionalStats is durationStats for sample sets that may legitimately be
// empty (TTFT): nil instead of an all-zero struct.
func optionalStats(samples []int64) *DurationStats {
	if len(samples) == 0 {
		return nil
	}
	s := durationStats(samples)
	return &s
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
