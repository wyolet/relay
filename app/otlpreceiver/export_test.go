package otlpreceiver

import "github.com/prometheus/client_golang/prometheus/testutil"

// ContentCount reads the content counter for one outcome.
func ContentCount(outcome string) float64 {
	return testutil.ToFloat64(contentTotal.WithLabelValues(outcome))
}

// RecordCount reads the record counter of a signal ("spans" or "log_records") for one outcome.
func RecordCount(signal, outcome string) float64 {
	counter := spansTotal
	if signal == "log_records" {
		counter = logRecordsTotal
	}
	return testutil.ToFloat64(counter.WithLabelValues(outcome))
}
