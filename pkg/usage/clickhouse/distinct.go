package clickhouse

import (
	"fmt"

	"github.com/wyolet/relay/pkg/usage"
)

// distinctSelect is the trailing SELECT entry counting dim's distinct non-empty values, "" when dim is unset. The column comes from the usage.DistinctColumn allowlist.
func distinctSelect(dim string) (string, error) {
	if dim == "" {
		return "", nil
	}
	col, ok := usage.DistinctColumn(dim)
	if !ok {
		return "", fmt.Errorf("usage/clickhouse: invalid distinct %q", dim)
	}
	return fmt.Sprintf(",\n    toInt64(uniqExactIf(%s, %s != '')) AS distinct_count", col, col), nil
}

// distinctCount is the scanned distinct_count as SummaryRow/TimeSeriesPoint carry it: nil when the query asked for none.
func distinctCount(dim string, n int64) *int64 {
	if dim == "" {
		return nil
	}
	return &n
}
