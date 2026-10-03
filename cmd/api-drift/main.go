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
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

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

	cfg := loadConfig(*cfgPath)
	state := loadState(*statePath)
	hints := fetchHints(cfg)
	results := runProbes(&http.Client{Timeout: *timeout}, cfg, hints)

	now := time.Now().UTC().Format(time.RFC3339)
	drift := applyState(state, results, now)
	printReport(results)
	saveState(*statePath, state)

	if *findingsPath != "" {
		writeFindings(*findingsPath, results, now)
	}
	if drift > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "api-drift:", err)
	os.Exit(2)
}
