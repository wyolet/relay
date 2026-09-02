package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"
)

// runToken implements `relay token mint`. Tokens are minted for a user in a
// project, so this logs in with a password and carries the session cookie —
// the admin token identifies no user and cannot mint one.
func runToken(args []string) error {
	if len(args) == 0 || args[0] != "mint" {
		return fmt.Errorf("usage: relay token mint --project <slug> [--ttl 1h]")
	}
	fs := flag.NewFlagSet("token mint", flag.ContinueOnError)
	project := fs.String("project", "", "Project slug the token is scoped to.")
	ttl := fs.String("ttl", "", "Token lifetime, e.g. 1h. Server default applies when empty.")
	username := fs.String("user", "", "Username. Default $RELAY_USER.")
	password := fs.String("password", "", "Password. Default $RELAY_PASSWORD.")
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("token mint: --project is required")
	}
	user, pass := orEnv(*username, "RELAY_USER"), orEnv(*password, "RELAY_PASSWORD")
	if user == "" || pass == "" {
		return fmt.Errorf("token mint: --user and --password (or $RELAY_USER / $RELAY_PASSWORD) are required")
	}

	base := orEnv(*serverURL, "RELAY_URL")
	if base == "" {
		base = DefaultControlURL
	}
	base = strings.TrimSuffix(base, "/")

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	if _, err := postJSON(c, base+"/api/auth/login",
		map[string]string{"username": user, "password": pass}); err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("token mint: login: %w", err)}
	}

	req := map[string]string{"project": *project}
	if *ttl != "" {
		req["ttl"] = *ttl
	}
	body, err := postJSON(c, base+"/api/auth/token", req)
	if err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("token mint: %w", err)}
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		_, _ = os.Stdout.Write(body)
		return nil
	}
	fmt.Println(out.Token)
	return nil
}

func postJSON(c *http.Client, url string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, &httpError{Status: resp.StatusCode, Body: out}
	}
	return out, nil
}

func orEnv(v, envKey string) string {
	if v != "" {
		return v
	}
	return os.Getenv(envKey)
}
