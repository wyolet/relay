package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
