package usage

// ValidDistinct lists the dimensions SummaryQuery.Distinct and TimeSeriesQuery.Distinct may count. Each is also the column name in the SQL schemas.
var ValidDistinct = []string{
	"relay_key_hash",
	"relay_key",
	"principal_id",
	"credential_id",
	"project_id",
	"team_id",
	"model_id",
	"host_id",
}

// DistinctColumn returns the column counted for a ValidDistinct dimension; ok is false for anything else. SQL backends embed only the returned allowlist entry, never the caller's string.
func DistinctColumn(dim string) (column string, ok bool) {
	for _, c := range ValidDistinct {
		if c == dim {
			return c, true
		}
	}
	return "", false
}

// distinctValues collects the non-empty values of one dimension seen in a row or point; nil when no distinct dimension was asked for.
type distinctValues map[string]struct{}

func newDistinctValues(dim string) distinctValues {
	if dim == "" {
		return nil
	}
	return distinctValues{}
}

func (d distinctValues) add(ev Event, dim string) {
	if d == nil {
		return
	}
	if v := groupKey(ev, dim); v != "" {
		d[v] = struct{}{}
	}
}

func (d distinctValues) count() *int64 {
	if d == nil {
		return nil
	}
	n := int64(len(d))
	return &n
}
