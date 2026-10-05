package chttl

import "testing"

func TestParseDays(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		want   int
	}{
		{"no ttl", "MergeTree PARTITION BY toYYYYMMDD(ts) ORDER BY (ts, request_id) SETTINGS index_granularity = 8192", 0},
		{"day ttl", "MergeTree PARTITION BY toYYYYMMDD(ts) ORDER BY (ts, request_id) TTL toDateTime(ts) + toIntervalDay(30) SETTINGS index_granularity = 8192", 30},
		{"day ttl at end", "MergeTree ORDER BY ts TTL toDateTime(ts) + toIntervalDay(7)", 7},
		{"other column", "MergeTree ORDER BY ts TTL toDateTime(created) + toIntervalDay(7)", -1},
		{"other unit", "MergeTree ORDER BY ts TTL toDateTime(ts) + toIntervalHour(12) SETTINGS index_granularity = 8192", -1},
		{"extra action", "MergeTree ORDER BY ts TTL toDateTime(ts) + toIntervalDay(7), toDateTime(ts) + toIntervalDay(1) TO VOLUME 'cold' SETTINGS index_granularity = 8192", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDays(c.engine, "ts"); got != c.want {
				t.Fatalf("parseDays = %d, want %d", got, c.want)
			}
		})
	}
}
