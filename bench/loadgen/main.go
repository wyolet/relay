// Command loadgen sends a steady-rate stream of /v1/messages requests at a
// Relay endpoint. Designed for stressing the eventlog/CH path: defaults
// target the loadtest route configured against the fakeanthropic upstream.
//
// Usage:
//
//	go run ./bench/loadgen --rps 50 --duration 2m
//
// Flags:
//
//	--target    base URL of the relay (default http://localhost:5100)
//	--api-key   client API key (default test-key)
//	--model     model name to send in body (default loadtest-model)
//	--rps       requests/sec (default 50)
//	--duration  total run length (default 2m)
//	--timeout   per-request timeout (default 30s — long enough for 20s upstream)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		target   = flag.String("target", "http://localhost:5100", "base URL of relay (nginx)")
		apiKey   = flag.String("api-key", "test-key", "client x-api-key for relay")
		model    = flag.String("model", "loadtest-model", "model name in request body")
		rps      = flag.Int("rps", 50, "request rate per second")
		duration = flag.Duration("duration", 2*time.Minute, "total run duration")
		timeout  = flag.Duration("timeout", 30*time.Second, "per-request timeout")
	)
	flag.Parse()

	if *rps <= 0 {
		log.Fatal("rps must be > 0")
	}

	body := mustMarshal(map[string]any{
		"model":      *model,
		"max_tokens": 256,
		"messages": []map[string]string{
			{"role": "user", "content": "ping from loadgen — please respond"},
		},
	})

	url := *target + "/v1/messages"
	client := &http.Client{Timeout: *timeout}

	var (
		sent       atomic.Int64
		ok2xx      atomic.Int64
		failOther  atomic.Int64
		failTimeout atomic.Int64
		latMu      sync.Mutex
		latencies  []time.Duration
	)

	ctx, cancel := context.WithTimeout(context.Background(), *duration+10*time.Second)
	defer cancel()

	tickInterval := time.Second / time.Duration(*rps)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(*duration)
	var wg sync.WaitGroup

	log.Printf("loadgen → %s model=%s rps=%d duration=%s", url, *model, *rps, *duration)

	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-progress.C:
				log.Printf("progress: sent=%d ok=%d fail_other=%d fail_timeout=%d",
					sent.Load(), ok2xx.Load(), failOther.Load(), failTimeout.Load())
			}
		}
	}()

LOOP:
	for {
		select {
		case <-ctx.Done():
			break LOOP
		case now := <-ticker.C:
			if now.After(deadline) {
				break LOOP
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					failOther.Add(1)
					return
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-api-key", *apiKey)
				req.Header.Set("anthropic-version", "2023-06-01")

				sent.Add(1)
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						failTimeout.Add(1)
					} else {
						failOther.Add(1)
					}
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				lat := time.Since(start)
				latMu.Lock()
				latencies = append(latencies, lat)
				latMu.Unlock()

				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					ok2xx.Add(1)
				} else {
					failOther.Add(1)
				}
			}()
		}
	}

	log.Printf("send loop ended; waiting for in-flight…")
	wg.Wait()

	latMu.Lock()
	defer latMu.Unlock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}

	fmt.Printf("\n--- loadgen summary ---\n")
	fmt.Printf("sent:           %d\n", sent.Load())
	fmt.Printf("ok 2xx:         %d\n", ok2xx.Load())
	fmt.Printf("fail other:     %d\n", failOther.Load())
	fmt.Printf("fail timeout:   %d\n", failTimeout.Load())
	fmt.Printf("latency p50:    %s\n", pct(0.50))
	fmt.Printf("latency p95:    %s\n", pct(0.95))
	fmt.Printf("latency p99:    %s\n", pct(0.99))
	fmt.Printf("latency max:    %s\n", pct(1.0))
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatal(err)
	}
	return b
}
