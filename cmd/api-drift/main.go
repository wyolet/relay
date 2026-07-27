// Command api-drift probes upstream provider APIs for parameter-support
// drift: request parameters the adapters forward unconditionally that a
// model has stopped accepting. For each configured host×model it sends a
// baseline request, then one request per parameter, and classifies the
// upstream verdict. Registry hints (models.dev, OpenRouter) are fetched as
// corroborating witnesses but never trusted alone — a finding requires a
// live rejection. Observations persist to a state file so repeat runs
// distinguish new drift from known.
//
// The tool detects and testifies; it deliberately files nothing. Findings
// (-findings) are a structured evidence report — verbatim upstream errors,
// witness verdicts, repro payloads — for a reviewer (human or LLM) to
// analyze against the adapters and catalog before authoring an issue or a
// catalog patch. A rejection message often carries nuance a mechanical
// filer would flatten (e.g. "only the default value is supported" is a
// value restriction, not an unsupported param).
//
// Probes are synthetic one-word prompts with tiny max-token caps; no
// customer data is involved anywhere in the chain. Exit codes: 0 no drift,
// 1 drift found, 2 run error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Registries struct {
		ModelsDev  string `yaml:"modelsdev"`
		OpenRouter string `yaml:"openrouter"`
	} `yaml:"registries"`
	Hosts []HostCfg `yaml:"hosts"`
}

type HostCfg struct {
	Name             string   `yaml:"name"`
	Shape            string   `yaml:"shape"` // openai | anthropic
	BaseURL          string   `yaml:"baseURL"`
	KeyEnv           string   `yaml:"keyEnv"`
	RegistryProvider string   `yaml:"registryProvider"` // models.dev top-level key
	ORPrefix         string   `yaml:"orPrefix"`         // OpenRouter model-id prefix
	Models           []string `yaml:"models"`
	Params           []string `yaml:"params"`
}

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

type StateEntry struct {
	Verdict   Verdict `json:"verdict"`
	Status    int     `json:"status"`
	Message   string  `json:"message,omitempty"`
	CheckedAt string  `json:"checkedAt"`
}

func main() {
	cfgPath := flag.String("config", "", "probe matrix YAML (required)")
	statePath := flag.String("state", ".tmp/drift/state.json", "observation state file")
	findingsPath := flag.String("findings", "", "write drift findings as a JSON evidence report")
	timeout := flag.Duration("timeout", 30*time.Second, "per-probe timeout")
	flag.Parse()
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "api-drift: -config is required")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fatal(err)
	}
	for _, h := range cfg.Hosts {
		if os.Getenv(h.KeyEnv) == "" {
			fatal(fmt.Errorf("host %s: env %s is empty", h.Name, h.KeyEnv))
		}
		if h.Shape != "openai" && h.Shape != "anthropic" {
			fatal(fmt.Errorf("host %s: unknown shape %q", h.Name, h.Shape))
		}
	}

	state := loadState(*statePath)
	hints := fetchHints(cfg)
	client := &http.Client{Timeout: *timeout}

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

	now := time.Now().UTC().Format(time.RFC3339)
	drift := 0
	for i := range results {
		r := &results[i]
		key := stateKey(r.Host.Name, r.Model, r.Param)
		prev, seen := state[key]
		r.New = !seen || prev.Verdict != r.Verdict
		state[key] = StateEntry{Verdict: r.Verdict, Status: r.Status, Message: trunc(r.Message, 400), CheckedAt: now}
		if r.Verdict == VUnsupported {
			drift++
		}
	}
	printReport(results)
	saveState(*statePath, state)

	if *findingsPath != "" {
		writeFindings(*findingsPath, results, now)
	}
	if drift > 0 {
		os.Exit(1)
	}
}

// Finding is one confirmed drift observation with everything a reviewer
// needs to judge it: the upstream's verbatim verdict, the registry
// witnesses, and the exact probe that produced it.
type Finding struct {
	Host       string            `json:"host"`
	BaseURL    string            `json:"baseURL"`
	Shape      string            `json:"shape"`
	Model      string            `json:"model"`
	Param      string            `json:"param"`
	HTTPStatus int               `json:"httpStatus"`
	Message    string            `json:"message"`
	Witnesses  map[string]string `json:"witnesses,omitempty"`
	New        bool              `json:"new"`
	ProbePath  string            `json:"probePath"`
	ProbeBody  json.RawMessage   `json:"probeBody"`
}

func writeFindings(path string, results []Result, now string) {
	report := struct {
		GeneratedAt string    `json:"generatedAt"`
		Findings    []Finding `json:"findings"`
	}{GeneratedAt: now, Findings: []Finding{}}
	for _, r := range results {
		if r.Verdict != VUnsupported {
			continue
		}
		body, probePath, _ := buildRequest(r.Host, r.Model, r.Param)
		report.Findings = append(report.Findings, Finding{
			Host:       r.Host.Name,
			BaseURL:    r.Host.BaseURL,
			Shape:      r.Host.Shape,
			Model:      r.Model,
			Param:      r.Param,
			HTTPStatus: r.Status,
			Message:    r.Message,
			Witnesses:  r.Hints,
			New:        r.New,
			ProbePath:  probePath,
			ProbeBody:  json.RawMessage(body),
		})
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("findings report: %s (%d finding(s))\n", path, len(report.Findings))
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

// --- registry hints ---

type hintSet struct {
	mdTemp map[string]*bool    // "provider/model" → models.dev temperature flag
	orSupp map[string][]string // OpenRouter id → supported_parameters
}

func fetchHints(cfg Config) *hintSet {
	hs := &hintSet{mdTemp: map[string]*bool{}, orSupp: map[string][]string{}}
	if u := cfg.Registries.ModelsDev; u != "" {
		var doc map[string]struct {
			Models map[string]struct {
				Temperature *bool `json:"temperature"`
			} `json:"models"`
		}
		if err := getJSON(u, &doc); err != nil {
			fmt.Fprintln(os.Stderr, "warn: models.dev fetch:", err)
		} else {
			for prov, p := range doc {
				for id, m := range p.Models {
					hs.mdTemp[prov+"/"+id] = m.Temperature
				}
			}
		}
	}
	if u := cfg.Registries.OpenRouter; u != "" {
		var doc struct {
			Data []struct {
				ID                  string   `json:"id"`
				SupportedParameters []string `json:"supported_parameters"`
			} `json:"data"`
		}
		if err := getJSON(u, &doc); err != nil {
			fmt.Fprintln(os.Stderr, "warn: openrouter fetch:", err)
		} else {
			for _, m := range doc.Data {
				hs.orSupp[m.ID] = m.SupportedParameters
			}
		}
	}
	return hs
}

func (hs *hintSet) for_(h HostCfg, model, param string) map[string]string {
	out := map[string]string{}
	if param == "temperature" && h.RegistryProvider != "" {
		if t, ok := hs.mdTemp[h.RegistryProvider+"/"+model]; ok && t != nil {
			out["models.dev"] = map[bool]string{true: "supported", false: "unsupported"}[*t]
		} else {
			out["models.dev"] = "unknown"
		}
	}
	if h.ORPrefix != "" {
		if supp, ok := hs.orSupp[h.ORPrefix+"/"+model]; ok {
			out["openrouter"] = "unsupported"
			for _, s := range supp {
				if s == param {
					out["openrouter"] = "supported"
				}
			}
		} else {
			out["openrouter"] = "unknown"
		}
	}
	return out
}

func getJSON(url string, v any) error {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// --- report / state / issues ---

func printReport(results []Result) {
	fmt.Printf("%-11s %-22s %-13s %-14s %-5s %s\n", "HOST", "MODEL", "PARAM", "VERDICT", "HTTP", "NOTE")
	drift := []Result{}
	for _, r := range results {
		p := r.Param
		if p == "" {
			p = "(baseline)"
		}
		verdict := string(r.Verdict)
		if r.Verdict == VUnsupported {
			verdict = "DRIFT"
			drift = append(drift, r)
		}
		flag := ""
		if r.New && r.Verdict == VUnsupported {
			flag = " [new]"
		}
		fmt.Printf("%-11s %-22s %-13s %-14s %-5d %s%s\n",
			r.Host.Name, r.Model, p, verdict, r.Status, trunc(r.Message, 90), flag)
	}
	fmt.Println()
	if len(drift) == 0 {
		fmt.Println("no drift detected")
		return
	}
	fmt.Printf("%d drift finding(s):\n", len(drift))
	for _, r := range drift {
		fmt.Printf("  %s/%s rejects %s (HTTP %d): %s\n", r.Host.Name, r.Model, r.Param, r.Status, trunc(r.Message, 200))
		for reg, verdict := range r.Hints {
			fmt.Printf("    witness %s: %s\n", reg, verdict)
		}
	}
}

func stateKey(host, model, param string) string {
	if param == "" {
		param = "_baseline"
	}
	return host + "/" + model + "/" + param
}

func loadState(path string) map[string]StateEntry {
	state := map[string]StateEntry{}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, &state)
	}
	return state
}

func saveState(path string, state map[string]StateEntry) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	raw, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "warn: state save:", err)
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "api-drift:", err)
	os.Exit(2)
}
