package usagetest

import (
	"time"

	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Start is the first included fixture event; every timestamp is a fixed offset from it so no result depends on the wall clock.
var Start = time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return Start.Add(time.Duration(minutes) * time.Minute) }

// Window is a filter spanning every fixture event, log-only ones included.
func Window() usage.EventQuery {
	return usage.EventQuery{From: at(-60), To: at(120), Limit: usage.MaxEventLimit}
}

// Fixture returns the events every backend is loaded with. Durations are equal within a model (model-a 200ms, model-b 100ms) and every TTFT truncates to 40ms except r03's 41ms, so percentiles agree across backends on any row holding one model and one TTFT. Some averages are fractional on purpose (key-a: 166.7ms duration, 41.3ms TTFT) to pin truncation on every backend. r00 and r11 are log-only and sit outside the included span, so a backend that aggregates them shifts counts, from/to and buckets. r05 carries no relay key. Only r01/r02 (project-a) and r06/r11 (principal-a) carry attribution, for read-scope cases.
func Fixture() []usage.Event {
	upstream := func(responseStartUs int64) *sdkusage.UpstreamTiming {
		return &sdkusage.UpstreamTiming{Start: 1_000, ResponseStart: responseStartUs, ResponseEnd: 90_000}
	}
	cost := func(n int64) *int64 { return &n }
	modelA := func(id string, minute int, source string, status int) usage.Event {
		return usage.Event{RequestID: id, Timestamp: at(minute), Source: source, Status: status,
			DurationMs: 200, Model: "model-a", Provider: "provider-a", RelayKey: "key-b"}
	}
	modelB := func(id string, minute int, source string, status int) usage.Event {
		return usage.Event{RequestID: id, Timestamp: at(minute), Source: source, Status: status,
			DurationMs: 100, Model: "model-b", Provider: "provider-b", RelayKey: "key-b"}
	}

	r01 := modelA("r01", 0, "pipeline", 200)
	r01.Upstream, r01.CostNanos, r01.RelayKey = upstream(40_999), cost(1000), "key-a"
	r01.Tokens = sdkusage.Tokens{"input": 10, "output": 5}
	r01.Tags = map[string]string{"env": "prod"}
	r01.ProjectID = "project-a"

	r02 := modelA("r02", 10, "pipeline", 200)
	r02.Upstream, r02.CostNanos, r02.RelayKey = upstream(40_999), cost(3000), "key-a"
	r02.Tokens = sdkusage.Tokens{"input": 20, "output": 10, "cache_read": 7}
	r02.Tags = map[string]string{"env": "prod"}
	r02.ProjectID = "project-a"

	r03 := modelB("r03", 20, "pipeline", 429)
	r03.Upstream, r03.RelayKey = upstream(41_999), "key-a"

	// Priced at a true $0: counts as priced, never as unpriced.
	r04 := modelB("r04", 30, "proxy", 404)
	r04.CostNanos = cost(0)

	r05 := modelA("r05", 45, "proxy", 200)
	r05.CostNanos, r05.RelayKey = cost(250), ""
	r05.Tokens = sdkusage.Tokens{"input": 1, "output": 2}

	r06 := modelB("r06", 65, "proxy", 200)
	r06.CostNanos = cost(2000)
	r06.Tokens = sdkusage.Tokens{"input": 30, "output": 15}
	r06.PrincipalID = "principal-a"

	r07 := modelA("r07", 75, "pipeline", 500)
	r07.Upstream = upstream(40_000)

	r08 := modelA("r08", 85, "proxy", 429)

	// Out-of-band status: still an error, and every backend files it as 5xx.
	r09 := modelB("r09", 95, "proxy", 600)

	r10 := modelA("r10", 110, "pipeline", 200)
	r10.Upstream, r10.CostNanos = upstream(40_000), cost(500)
	r10.Tokens = sdkusage.Tokens{"input": 5, "output": 5}

	r00 := modelA("r00", -1, "pipeline", 0)
	r00.DurationMs, r00.ErrorKind, r00.RelayKey = 0, "no_keys", "key-a"
	r11 := modelB("r11", 119, "proxy", 0)
	r11.DurationMs, r11.ErrorKind, r11.PrincipalID = 0, "rate_limited", "principal-a"

	return []usage.Event{r00, r01, r02, r03, r04, r05, r06, r07, r08, r09, r10, r11}
}
