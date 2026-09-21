package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Verdict string

const (
	VSupported     Verdict = "supported"
	VUnsupported   Verdict = "unsupported" // 4xx naming the param — drift
	VRejectedOther Verdict = "rejected_other"
	VInconclusive  Verdict = "inconclusive"
	VUnprobeable   Verdict = "unprobeable" // baseline failed
)

type Result struct {
	Host    HostCfg
	Model   string
	Param   string // "" = baseline
	Verdict Verdict
	Status  int
	Message string
	Hints   map[string]string // registry name → supported|unsupported|unknown
	New     bool
}

// runProbes probes every host in parallel (one goroutine per host, models
// sequential within a host to stay under per-key rate limits) and returns the
// results sorted by host/model/param.
func runProbes(client *http.Client, cfg Config, hints *hintSet) []Result {
	var (
		mu      sync.Mutex
		results []Result
		wg      sync.WaitGroup
	)
	for _, h := range cfg.Hosts {
		wg.Add(1)
		go func(h HostCfg) {
			defer wg.Done()
			for _, model := range h.Models {
				rs := probeModel(client, h, model, hints)
				mu.Lock()
				results = append(results, rs...)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Host.Name != b.Host.Name {
			return a.Host.Name < b.Host.Name
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Param < b.Param
	})
	return results
}

func probeModel(client *http.Client, h HostCfg, model string, hints *hintSet) []Result {
	base := Result{Host: h, Model: model, Hints: map[string]string{}}

	v, status, msg := probe(client, h, model, "")
	baseline := base
	baseline.Verdict, baseline.Status, baseline.Message = v, status, msg
	out := []Result{baseline}
	if v != VSupported {
		baseline.Verdict = VUnprobeable
		out[0] = baseline
		for _, p := range h.Params {
			r := base
			r.Param, r.Verdict = p, VInconclusive
			r.Message = "baseline failed: " + trunc(msg, 120)
			out = append(out, r)
		}
		return out
	}
	for _, p := range h.Params {
		v, status, msg := probe(client, h, model, p)
		r := base
		r.Param, r.Verdict, r.Status, r.Message = p, v, status, msg
		r.Hints = hints.for_(h, model, p)
		out = append(out, r)
	}
	return out
}

func probe(client *http.Client, h HostCfg, model, param string) (Verdict, int, string) {
	body, path, hdr := buildRequest(h, model, param)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", strings.TrimRight(h.BaseURL, "/")+path, strings.NewReader(body))
		if err != nil {
			return VInconclusive, 0, err.Error()
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return VInconclusive, 0, err.Error()
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode == 429 && attempt == 0 {
			time.Sleep(3 * time.Second)
			continue
		}
		return classify(param, resp.StatusCode, string(rb))
	}
}

func buildRequest(h HostCfg, model, param string) (body, path string, hdr map[string]string) {
	key := os.Getenv(h.KeyEnv)
	m := map[string]any{"model": model}
	switch h.Shape {
	case "anthropic":
		path = "/v1/messages"
		hdr = map[string]string{
			"content-type":      "application/json",
			"x-api-key":         key,
			"anthropic-version": "2023-06-01",
		}
		m["max_tokens"] = 64
	case "openai":
		path = "/v1/chat/completions"
		hdr = map[string]string{
			"content-type":  "application/json",
			"Authorization": "Bearer " + key,
		}
		m["max_completion_tokens"] = 64
	}
	m["messages"] = []map[string]any{{"role": "user", "content": "ping"}}
	switch param {
	case "":
	case "temperature":
		m["temperature"] = 0.7
	case "top_p":
		m["top_p"] = 0.9
	case "top_k":
		m["top_k"] = 5
	default:
		fatal(fmt.Errorf("unknown param %q", param))
	}
	b, _ := json.Marshal(m)
	return string(b), path, hdr
}

func classify(param string, status int, body string) (Verdict, int, string) {
	msg := extractMessage(body)
	if status >= 200 && status < 300 {
		return VSupported, status, ""
	}
	switch status {
	case 401, 403:
		return VInconclusive, status, "auth: " + msg
	case 429:
		return VInconclusive, status, "rate limited: " + msg
	}
	if status >= 500 {
		return VInconclusive, status, msg
	}
	if param != "" && strings.Contains(strings.ToLower(body), strings.ToLower(param)) {
		return VUnsupported, status, msg
	}
	return VRejectedOther, status, msg
}

// extractMessage digs the human message out of {"error":{"message":...}}
// (OpenAI and Anthropic both nest it there); falls back to the raw body.
func extractMessage(body string) string {
	var wire struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &wire) == nil && wire.Error.Message != "" {
		return wire.Error.Message
	}
	return strings.TrimSpace(body)
}
