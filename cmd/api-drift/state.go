package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type StateEntry struct {
	Verdict   Verdict `json:"verdict"`
	Status    int     `json:"status"`
	Message   string  `json:"message,omitempty"`
	CheckedAt string  `json:"checkedAt"`
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

// applyState records this run's verdicts against the prior observations,
// marking each result new when unseen or changed, and returns the drift count.
func applyState(state map[string]StateEntry, results []Result, now string) int {
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
	return drift
}
