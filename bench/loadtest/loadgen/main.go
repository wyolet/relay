// Command loadgen drives a constant-arrival-rate load at a relay endpoint and
// writes coordinated-omission-correct latency percentiles + throughput as JSON
// (for the harness report) and a human summary to stderr.
//
// Constant arrival rate (vegeta's pacer) is deliberate: requests are issued on
// a fixed schedule regardless of when responses return, so a saturating server
// can't mask its tail latency by slowing the offered load. It runs as its own
// container, off the relay hosts, so the generator never competes with relay
// for CPU.
//
// Standalone module — the vegeta dependency never reaches the relay binary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

type result struct {
	Name      string         `json:"name"`
	TargetRPS int            `json:"targetRps"`
	Requests  uint64         `json:"requests"`
	AchievedR float64        `json:"achievedRps"`
	Success   float64        `json:"success"`
	P50ms     float64        `json:"p50ms"`
	P90ms     float64        `json:"p90ms"`
	P99ms     float64        `json:"p99ms"`
	P999ms    float64        `json:"p999ms"`
	Maxms     float64        `json:"maxMs"`
	Meanms    float64        `json:"meanMs"`
	Codes     map[string]int `json:"codes"`
	Errors    []string       `json:"errors,omitempty"`
}

func main() {
	var (
		target   = flag.String("url", "", "full URL (required)")
		rate     = flag.Int("rate", 200, "requests/sec (constant arrival rate)")
		duration = flag.Duration("duration", 30*time.Second, "attack duration")
		method   = flag.String("method", "POST", "HTTP method")
		bodyFile = flag.String("body", "", "request body file (else a default chat-completions body)")
		model    = flag.String("model", "loadtest", "model name for the default body")
		key      = flag.String("key", "", "bearer token")
		name     = flag.String("name", "run", "scenario label")
		out      = flag.String("out", "", "write JSON result to this path (else stdout)")
		timeout  = flag.Duration("timeout", 60*time.Second, "per-request timeout")
	)
	flag.Parse()
	if *target == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -url required")
		os.Exit(2)
	}

	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
			os.Exit(1)
		}
		body = b
	} else {
		body = []byte(`{"model":"` + *model + `","messages":[{"role":"user","content":"ping from loadgen"}],"max_tokens":16}`)
	}

	header := map[string][]string{"Content-Type": {"application/json"}}
	if *key != "" {
		header["Authorization"] = []string{"Bearer " + *key}
	}
	targeter := vegeta.NewStaticTargeter(vegeta.Target{Method: *method, URL: *target, Body: body, Header: header})
	attacker := vegeta.NewAttacker(vegeta.Timeout(*timeout), vegeta.KeepAlive(true))
	pacer := vegeta.ConstantPacer{Freq: *rate, Per: time.Second}

	fmt.Fprintf(os.Stderr, "[%s] %s %s @ %d/s for %s\n", *name, *method, *target, *rate, *duration)
	var m vegeta.Metrics
	for res := range attacker.Attack(targeter, pacer, *duration, *name) {
		m.Add(res)
	}
	m.Close()

	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	codes := map[string]int{}
	for c, n := range m.StatusCodes {
		codes[c] = n
	}
	r := result{
		Name: *name, TargetRPS: *rate, Requests: m.Requests, AchievedR: m.Rate, Success: m.Success * 100,
		P50ms: ms(m.Latencies.P50), P90ms: ms(m.Latencies.P90), P99ms: ms(m.Latencies.P99),
		P999ms: ms(m.Latencies.P99), Maxms: ms(m.Latencies.Max), Meanms: ms(m.Latencies.Mean),
		Codes: codes, Errors: m.Errors,
	}
	// vegeta exposes P999 via Quantile
	r.P999ms = ms(m.Latencies.Quantile(0.999))

	fmt.Fprintf(os.Stderr, "  achieved %.0f/s  success %.1f%%  p50 %.2fms  p99 %.2fms  max %.2fms\n",
		r.AchievedR, r.Success, r.P50ms, r.P99ms, r.Maxms)

	enc := json.NewEncoder(os.Stdout)
	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		enc = json.NewEncoder(f)
		_ = w
	}
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}
