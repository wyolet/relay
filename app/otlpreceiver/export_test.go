package otlpreceiver

import "github.com/prometheus/client_golang/prometheus/testutil"

// ContentCount reads the content counter for one outcome.
func ContentCount(outcome string) float64 {
	return testutil.ToFloat64(contentTotal.WithLabelValues(outcome))
}
