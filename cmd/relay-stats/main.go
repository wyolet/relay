// relay-stats — thin HTTP client for the relay control plane's
// /usage/events + /usage/summary endpoints. Pretty-prints aggregates
// as text tables; emits JSON with --json for piping.
//
// Auth: bearer token from $RELAY_ADMIN_TOKEN (or --token).
// Control plane URL: $RELAY_CONTROL_URL (default http://127.0.0.1:8090),
// override with --url.
//
// Examples:
//
//	relay-stats events --since 1h
//	relay-stats summary --by model_id --since 24h
//	relay-stats summary --by relay_key_hash --since 7d --json | jq .
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wyolet/relay/app/usagelog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "events":
		runEvents(os.Args[2:])
	case "summary":
		runSummary(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `relay-stats — query relay usage events

Usage:
  relay-stats events  [--since 1h] [--limit 50] [--model_id …] [--source pipeline] [--json]
  relay-stats summary [--by source] [--since 1h] [--model_id …] [--json]

Common flags:
  --since DUR        time window: 30m, 1h, 24h, 7d (default 1h)
  --url URL          control plane base URL (env RELAY_CONTROL_URL, default http://127.0.0.1:8090)
  --token TOKEN      admin bearer (env RELAY_ADMIN_TOKEN)
  --json             emit raw JSON instead of a table

Filter flags (shared):
  --relay_key_hash, --policy_id, --model_id, --host_id
  --source (pipeline|proxy|ws|batch)
  --status_min, --status_max

summary-only:
  --by FIELD         source (default) | model_id | host_id | policy_id | relay_key_hash | host_key_id

events-only:
  --limit N          cap on rows returned (default 100, max 10000)`)
}

// --- events ---

func runEvents(args []string) {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	common := registerCommon(fs)
	limit := fs.Int("limit", 100, "")
	_ = fs.Parse(args)

	q := common.query()
	q.Set("limit", fmt.Sprintf("%d", *limit))

	body := doGet(*common.url+"/usage/events?"+q.Encode(), *common.token)
	if *common.jsonOut {
		os.Stdout.Write(body)
		return
	}
	var resp struct {
		Events []usagelog.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		die("decode: %v", err)
	}
	printEvents(resp.Events)
}

// --- summary ---

func runSummary(args []string) {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	common := registerCommon(fs)
	by := fs.String("by", "source", "")
	_ = fs.Parse(args)

	q := common.query()
	q.Set("group_by", *by)

	body := doGet(*common.url+"/usage/summary?"+q.Encode(), *common.token)
	if *common.jsonOut {
		os.Stdout.Write(body)
		return
	}
	var result usagelog.SummaryResult
	if err := json.Unmarshal(body, &result); err != nil {
		die("decode: %v", err)
	}
	printSummary(result, *by)
}

// --- shared flag plumbing ---

type commonFlags struct {
	url, token *string
	since      *string
	jsonOut    *bool
	keyHash    *string
	policyID   *string
	modelID    *string
	hostID     *string
	source     *string
	statusMin  *int
	statusMax  *int
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	defUrl := os.Getenv("RELAY_CONTROL_URL")
	if defUrl == "" {
		defUrl = "http://127.0.0.1:8090"
	}
	c := commonFlags{
		url:       fs.String("url", defUrl, ""),
		token:     fs.String("token", os.Getenv("RELAY_ADMIN_TOKEN"), ""),
		since:     fs.String("since", "1h", ""),
		jsonOut:   fs.Bool("json", false, ""),
		keyHash:   fs.String("relay_key_hash", "", ""),
		policyID:  fs.String("policy_id", "", ""),
		modelID:   fs.String("model_id", "", ""),
		hostID:    fs.String("host_id", "", ""),
		source:    fs.String("source", "", ""),
		statusMin: fs.Int("status_min", 0, ""),
		statusMax: fs.Int("status_max", 0, ""),
	}
	return c
}

func (c commonFlags) query() url.Values {
	v := url.Values{}
	addIfNonEmpty := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	addIfNonEmpty("since", *c.since)
	addIfNonEmpty("relay_key_hash", *c.keyHash)
	addIfNonEmpty("policy_id", *c.policyID)
	addIfNonEmpty("model_id", *c.modelID)
	addIfNonEmpty("host_id", *c.hostID)
	addIfNonEmpty("source", *c.source)
	if *c.statusMin > 0 {
		v.Set("status_min", fmt.Sprintf("%d", *c.statusMin))
	}
	if *c.statusMax > 0 {
		v.Set("status_max", fmt.Sprintf("%d", *c.statusMax))
	}
	return v
}

// --- HTTP plumbing ---

func doGet(u, token string) []byte {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		die("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("call %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		die("HTTP %d from %s: %s", resp.StatusCode, u, string(body))
	}
	return body
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relay-stats: "+format+"\n", args...)
	os.Exit(1)
}

// --- pretty printers ---

func printEvents(events []usagelog.Event) {
	if len(events) == 0 {
		fmt.Println("(no events)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "TS\tSOURCE\tSTATUS\tDUR_MS\tMODEL\tHOST\tIN\tOUT\tKEY")
	for _, ev := range events {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%d\t%d\t%s\n",
			ev.Timestamp.Format(time.RFC3339),
			ev.Source,
			ev.Status,
			ev.DurationMs,
			short(ev.ModelID),
			short(ev.HostID),
			ev.Tokens["input"],
			ev.Tokens["output"],
			short(ev.RelayKeyHash),
		)
	}
}

func printSummary(res usagelog.SummaryResult, by string) {
	if len(res.Rows) == 0 {
		fmt.Println("(no events in window)")
		return
	}
	if !res.From.IsZero() {
		fmt.Printf("# %s → %s\n", res.From.Format(time.RFC3339), res.To.Format(time.RFC3339))
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	header := strings.ToUpper(by)
	fmt.Fprintf(w, "%s\tREQS\tERRORS\tIN\tOUT\tCACHE_RD\tAVG_MS\tP50\tP95\tP99\n", header)
	for _, row := range res.Rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			short(row.Group[by]),
			row.Requests,
			row.ErrorCount,
			row.Tokens["input"],
			row.Tokens["output"],
			row.Tokens["cache_read"],
			row.DurationMs.Avg,
			row.DurationMs.P50,
			row.DurationMs.P95,
			row.DurationMs.P99,
		)
	}
}

// short truncates UUID-shaped strings to the first 8 chars so the
// table stays readable.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// silence unused import on platforms where sort isn't pulled in via
// the printers above.
var _ = sort.Strings
