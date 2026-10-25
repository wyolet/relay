package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/manifest"
)

// exitError lets a subcommand choose its process exit code. `apply` needs
// three: clean, drift, and a failed request.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// Exit codes for `relay apply`, so CI can tell drift from failure.
const (
	exitDrift  = 1 // at least one row was left alone as operator-edited
	exitFailed = 2 // the server refused the apply
)

type applyResponse struct {
	Plan    []apply.Entry `json:"plan"`
	Applied bool          `json:"applied"`
	Counts  apply.Counts  `json:"counts"`
}

// runApply implements `relay apply -f <file|dir>`.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	from := fs.String("f", "", "Manifest file or directory to apply.")
	dryRun := fs.Bool("dry-run", false, "Print the plan; write nothing.")
	force := fs.Bool("force", false, "Write over operator-edited (dirty) rows and groups a provisioning source manages.")
	prune := fs.Bool("prune", false, "Delete selected rows the bundle omits. Requires --selector.")
	selector := fs.String("selector", "", "Label selector naming the managed set, e.g. env=prod,team=platform.")
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	token := fs.String("token", "", "Admin bearer token. Default $RELAY_ADMIN_TOKEN.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("apply: -f <file|dir> is required")
	}
	// A dotenv token would write to whatever deployment the file names; the operator must name the target.
	if *serverURL == "" && *token == "" && dotEnvKeys["RELAY_ADMIN_TOKEN"] {
		return fmt.Errorf("apply: RELAY_ADMIN_TOKEN comes from %s; pass --url to confirm the target", dotEnvFile)
	}

	body, err := loadBundle(*from)
	if err != nil {
		return err
	}

	q := url.Values{}
	if *dryRun {
		q.Set("dryRun", "true")
	}
	if *force {
		q.Set("force", "true")
	}
	if *prune {
		q.Set("prune", "true")
	}
	if *selector != "" {
		q.Set("selector", *selector)
	}

	client := newControlClient(*serverURL, *token)
	fmt.Fprintf(os.Stderr, "apply: target %s (%s)\n", client.baseURL, applyTargetSource(*serverURL))
	raw, err := client.do("POST", "/api/apply?"+q.Encode(), "application/yaml", body)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}

	var resp applyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("apply: decode response: %w", err)}
	}
	printPlan(os.Stdout, resp)

	// A forbidden row is one the caller may not read, so it was ignored:
	// CI must learn the tree was only partly applied.
	forbidden := 0
	for _, e := range resp.Plan {
		if e.Action == apply.ActionForbidden {
			forbidden++
		}
	}
	if forbidden > 0 {
		return &exitError{code: exitDrift, err: fmt.Errorf(
			"apply: %d row(s) are outside your scope and were ignored", forbidden)}
	}

	// A conflict is a row that changed between plan and write and was
	// skipped: the tree is not applied, so the run must not report success.
	conflicts := 0
	for _, e := range resp.Plan {
		if e.Action == apply.ActionConflict {
			conflicts++
		}
	}
	if conflicts > 0 {
		return &exitError{code: exitDrift, err: fmt.Errorf(
			"apply: %d row(s) changed between plan and write and were skipped; re-run to converge",
			conflicts)}
	}
	// The server plans a dirty row the manifest matches as unchanged, so every skip-dirty entry is one that differs.
	differing := 0
	for _, e := range resp.Plan {
		if e.Action == apply.ActionSkipDirty {
			differing++
		}
	}
	if differing > 0 && !*force {
		return &exitError{code: exitDrift, err: fmt.Errorf(
			"apply: %d row(s) differ from the manifest and were left alone; re-run with --force to overwrite",
			differing)}
	}
	return nil
}

// applyTargetSource names where the control URL came from, so the operator sees a target picked up from a file.
func applyTargetSource(flagURL string) string {
	switch {
	case flagURL != "":
		return "--url"
	case os.Getenv("RELAY_URL") == "":
		return "default"
	case dotEnvKeys["RELAY_URL"]:
		return "RELAY_URL from " + dotEnvFile
	default:
		return "RELAY_URL"
	}
}

// loadBundle parses the manifests locally — so a typo fails before the round
// trip — and re-renders them as the multi-document body the server reads.
func loadBundle(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var docs []manifest.Document
	if info.IsDir() {
		docs, err = manifest.LoadDir(path)
	} else {
		f, ferr := os.Open(filepath.Clean(path))
		if ferr != nil {
			return nil, ferr
		}
		defer f.Close()
		docs, err = manifest.Parse(f)
	}
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("apply: no manifest documents found in %s", path)
	}
	return manifest.Render(docs, "")
}

func printPlan(w *os.File, resp applyResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME\tACTION\tCHANGED")
	for _, e := range resp.Plan {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Kind, e.Name, e.Action, strings.Join(e.ChangedFields, ","))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\ncreate=%d update=%d unchanged=%d skip-dirty=%d delete=%d conflict=%d forbidden=%d applied=%v\n",
		resp.Counts.Create, resp.Counts.Update, resp.Counts.Unchanged,
		resp.Counts.SkipDirty, resp.Counts.Delete, resp.Counts.Conflict,
		resp.Counts.Forbidden, resp.Applied)
}
