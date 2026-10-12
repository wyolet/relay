package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// exitDenied is `relay auth can-i`'s exit code for a refused action.
const exitDenied = 1

const canIUsage = "usage: relay auth can-i <action> <kind>[/<name>] [--as <user>|sa:<name>] [--owner <kind>/<name>] [-o json]"

type ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// canIAnswer is the part of the access review the CLI prints.
type canIAnswer struct {
	Allowed          bool   `json:"allowed"`
	Reason           string `json:"reason"`
	Rule             string `json:"rule"`
	CredentialDenied bool   `json:"credentialDenied"`
	Action           string `json:"action"`
	Subject          struct {
		Kind     string `json:"kind"`
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
	} `json:"subject"`
	Resource struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Owner *ref   `json:"owner"`
	} `json:"resource"`
	Binding *struct {
		Name    string `json:"name"`
		Subject ref    `json:"subject"`
		Role    ref    `json:"role"`
		Scope   ref    `json:"scope"`
	} `json:"binding"`
}

// runAuth implements `relay auth can-i`: it asks the control API whether a subject may perform an action, prints why, and exits 0 when allowed and 1 when not.
func runAuth(args []string) error {
	if len(args) == 0 || args[0] != "can-i" {
		return &exitError{code: exitFailed, err: errors.New(canIUsage)}
	}
	fs := flag.NewFlagSet("auth can-i", flag.ContinueOnError)
	as := fs.String("as", "", "Check this user, or sa:<name> for a service account. Default: the caller.")
	owner := fs.String("owner", "", "Owner of the collection a list or create is about, as <kind>/<name> (project/ml, team/platform, user/alice, system).")
	output := fs.String("o", "", "json prints the server's answer as is.")
	serverURL := fs.String("url", "", "Control API base URL. Default $RELAY_URL, else "+DefaultControlURL+".")
	token := fs.String("token", "", "Admin bearer token. Default $RELAY_ADMIN_TOKEN.")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	if len(pos) != 2 {
		return &exitError{code: exitFailed, err: errors.New(canIUsage)}
	}
	body, err := canIRequest(pos[0], pos[1], *as, *owner)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	raw, err := newControlClient(*serverURL, *token).do("POST", "/api/access-review", "application/json", body)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	var answer canIAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("auth can-i: decode response: %w", err)}
	}
	if *output == "json" {
		_, _ = os.Stdout.Write(raw)
	} else {
		printCanI(os.Stdout, answer)
	}
	if !answer.Allowed {
		return &exitError{code: exitDenied, err: errors.New("denied")}
	}
	return nil
}

// canIRequest renders the access-review body from the command line.
func canIRequest(action, resource, as, owner string) ([]byte, error) {
	kind, name, _ := strings.Cut(resource, "/")
	if kind == "" {
		return nil, errors.New(canIUsage)
	}
	req := map[string]any{"action": action}
	res := map[string]any{"kind": kind}
	if name != "" {
		res["name"] = name
	}
	if owner != "" {
		ok, on, _ := strings.Cut(owner, "/")
		o := map[string]string{"kind": ok}
		if on != "" {
			o["name"] = on
		}
		res["owner"] = o
	}
	req["resource"] = res
	if as != "" {
		subject := map[string]string{"kind": "user", "name": as}
		for _, prefix := range []string{"sa:", "serviceaccount:"} {
			if n, ok := strings.CutPrefix(as, prefix); ok {
				subject = map[string]string{"kind": "serviceaccount", "name": n}
			}
		}
		if n, ok := strings.CutPrefix(as, "user:"); ok {
			subject["name"] = n
		}
		req["subject"] = subject
	}
	return json.Marshal(req)
}

func printCanI(w io.Writer, a canIAnswer) {
	verdict := "no"
	if a.Allowed {
		verdict = "yes"
	}
	_, _ = fmt.Fprintf(w, "%s: %s\n", verdict, canIReason(a))
	if a.Subject.Disabled {
		_, _ = fmt.Fprintf(w, "note: %s %s is disabled and cannot sign in\n", a.Subject.Kind, a.Subject.Name)
	}
}

func canIReason(a canIAnswer) string {
	what := a.Action + " on " + describe(ref{Kind: a.Resource.Kind, Name: a.Resource.Name})
	if a.Resource.Name == "" && a.Resource.Owner != nil {
		what += " in " + describe(*a.Resource.Owner)
	}
	granted := ""
	switch {
	case a.Binding != nil:
		b := a.Binding
		granted = fmt.Sprintf("role binding %s grants %s at %s to %s", b.Name, b.Role.Name, describe(b.Scope), describe(b.Subject))
	case a.Rule != "":
		granted = "rule " + a.Rule
	}
	switch a.Reason {
	case "admin":
		return a.Subject.Name + " is an admin"
	case "rule", "binding":
		if a.Allowed {
			return granted + " (" + what + ")"
		}
		return granted + " refuses " + what
	case "credential":
		return granted + ", but the credential's limits refuse " + what
	}
	return "no rule or role binding grants " + what
}

// describe renders a ref as "<kind> <name>", the global scope as "global".
func describe(r ref) string {
	switch {
	case r.Kind == "system":
		return "global"
	case r.Name != "":
		return r.Kind + " " + r.Name
	case r.ID != "":
		return r.Kind + " " + r.ID
	}
	return r.Kind
}

// parseInterspersed parses fs's flags wherever they appear among args and returns the positional arguments in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
