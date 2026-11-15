package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/wyolet/relay/app/catalogprune"
)

const catalogUsage = "usage: relay catalog prune [--apply] [--idle 30d]"

type catalogPruneResponse struct {
	DryRun       bool                    `json:"dryRun"`
	Idle         string                  `json:"idle"`
	Pruned       int                     `json:"pruned"`
	KeptDetached int                     `json:"keptDetached"`
	Rows         []catalogprune.Decision `json:"rows"`
}

// runCatalog implements `relay catalog prune`: a dry run unless --apply.
func runCatalog(args []string) error {
	if len(args) == 0 || args[0] != "prune" {
		return fmt.Errorf("%s", catalogUsage)
	}
	fs := flag.NewFlagSet("catalog prune", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "Delete the rows decided prune. Without it nothing is deleted.")
	idle := fs.String("idle", "", "How long a model or host binding must have served no request, e.g. 30d or 720h. Default: the relay's RELAY_CATALOG_PRUNE_IDLE.")
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	token := fs.String("token", "", "Admin bearer token. Default $RELAY_ADMIN_TOKEN.")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// A dotenv token would delete from whatever deployment the file names; the operator must name the target.
	if *apply && *serverURL == "" && *token == "" && dotEnvKeys["RELAY_ADMIN_TOKEN"] {
		return fmt.Errorf("catalog prune: RELAY_ADMIN_TOKEN comes from %s; pass --url to confirm the target", dotEnvFile)
	}

	q := url.Values{}
	q.Set("dryRun", strconv.FormatBool(!*apply))
	if *idle != "" {
		q.Set("idle", *idle)
	}
	client := newControlClient(*serverURL, *token)
	raw, err := client.do("POST", "/api/catalog/prune?"+q.Encode(), "", nil)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	var resp catalogPruneResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("catalog prune: decode response: %w", err)}
	}
	printPruneDecisions(os.Stdout, resp)
	return nil
}

func printPruneDecisions(w io.Writer, resp catalogPruneResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAME\tDECISION\tREASON")
	for _, d := range resp.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Kind, d.Name, d.Decision, d.Reason)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\npruned=%d kept-detached=%d idle=%s applied=%v\n", resp.Pruned, resp.KeptDetached, resp.Idle, !resp.DryRun)
}
