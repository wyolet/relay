package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
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
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("token mint: --project is required")
	}
	user := orEnv(*username, "RELAY_USER")
	if user == "" {
		return fmt.Errorf("token mint: --user (or $RELAY_USER) is required")
	}
	// No password flag: argv is world-readable through the process table and
	// lands in shell history.
	pass, err := readPassword()
	if err != nil {
		return err
	}

	base := orEnv(*serverURL, "RELAY_URL")
	if base == "" {
		base = DefaultControlURL
	}
	base = strings.TrimSuffix(base, "/")

	c := &http.Client{Timeout: 30 * time.Second}

	_, cookies, err := postJSON(c, base+"/api/auth/login", "",
		map[string]string{"username": user, "password": pass})
	if err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("token mint: login: %w", err)}
	}
	session := cookieHeader(cookies)
	// The session exists only to mint; drop it server-side rather than
	// leaving it to expire on its own.
	defer func() { _, _, _ = postJSON(c, base+"/api/auth/logout", session, nil) }()

	req := map[string]string{"project": *project}
	if *ttl != "" {
		req["ttl"] = *ttl
	}
	body, _, err := postJSON(c, base+"/api/auth/token", session, req)
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

// readPassword takes the password from $RELAY_PASSWORD, else prompts on the
// terminal with echo off. A non-interactive run with no env var is an error
// rather than a silent empty password.
func readPassword() (string, error) {
	if p := os.Getenv("RELAY_PASSWORD"); p != "" {
		return p, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("token mint: set $RELAY_PASSWORD (no terminal to prompt on)")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("token mint: read password: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("token mint: empty password")
	}
	return string(raw), nil
}

// postJSON posts body as JSON, carrying cookie verbatim when non-empty and
// returning the cookies the response set. The session cookie is passed by
// hand rather than through a cookiejar: the server marks it Secure, which a
// jar refuses to replay over a plain-http control listener.
func postJSON(c *http.Client, url, cookie string, body any) ([]byte, []*http.Cookie, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, url, payload)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, resp.Cookies(), &httpError{Status: resp.StatusCode, Body: out}
	}
	return out, resp.Cookies(), nil
}

// cookieHeader renders the cookies a response set as a request Cookie
// header value.
func cookieHeader(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

func orEnv(v, envKey string) string {
	if v != "" {
		return v
	}
	return os.Getenv(envKey)
}
