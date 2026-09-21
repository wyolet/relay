package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"

	"github.com/wyolet/relay/app/key"
)

// runExport implements `relay export`.
func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	scope := fs.String("scope", "", "Restrict to a subtree: team:<id> or project:<id>.")
	kinds := fs.String("kinds", "", "Comma-separated API plurals to include. Default: every exportable kind.")
	format := fs.String("format", "", "yaml (default) or json.")
	out := fs.String("o", "", "Write to this file instead of stdout.")
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	token := fs.String("token", "", "Admin bearer token. Default $RELAY_ADMIN_TOKEN.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := url.Values{}
	for k, v := range map[string]string{"scope": *scope, "kinds": *kinds, "format": *format} {
		if v != "" {
			q.Set(k, v)
		}
	}
	body, err := newControlClient(*serverURL, *token).do("GET", "/api/export?"+q.Encode(), "", nil)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	if *out == "" {
		_, err = os.Stdout.Write(body)
		return err
	}
	return os.WriteFile(*out, body, 0o600)
}

// runKeygen implements `relay keygen`: mints a key locally so an operator can
// put the hash in a manifest and keep the plaintext out of git. No server.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	prefixOnly := fs.Bool("prefix", false, "Print only the display prefix.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := key.Generate()
	if err != nil {
		return err
	}
	if *prefixOnly {
		fmt.Println(g.Prefix)
		return nil
	}
	fmt.Printf("plaintext: %s\nkeyHash:   %s\nprefix:    %s\n", g.Plaintext, g.KeyHash, g.Prefix)
	return nil
}
