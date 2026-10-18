package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const secretUsage = "usage: relay secret status | migrate | downgrade | rotate (--scope <scope> | --all) | rewrap --to <kek id> | destroy <data key id> [--force]"

// secretCommand is a parsed `relay secret` invocation.
type secretCommand struct {
	name  string
	scope string
	all   bool
	to    string
	id    string
	force bool
}

// parseSecretArgs parses and checks the arguments before anything touches the database.
func parseSecretArgs(args []string) (secretCommand, error) {
	if len(args) == 0 {
		return secretCommand{}, errors.New(secretUsage)
	}
	cmd := secretCommand{name: args[0]}
	fs := flag.NewFlagSet("secret "+cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch cmd.name {
	case "status", "migrate", "downgrade":
	case "rotate":
		fs.StringVar(&cmd.scope, "scope", "", "")
		fs.BoolVar(&cmd.all, "all", false, "")
	case "rewrap":
		fs.StringVar(&cmd.to, "to", "", "")
	case "destroy":
		fs.BoolVar(&cmd.force, "force", false, "")
	default:
		return secretCommand{}, errors.New(secretUsage)
	}
	rest := args[1:]
	// flag stops at the first positional argument, so the id may come before or after --force.
	if cmd.name == "destroy" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		cmd.id, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return secretCommand{}, fmt.Errorf("secret %s: %w\n%s", cmd.name, err, secretUsage)
	}
	if cmd.name == "destroy" && cmd.id == "" && fs.NArg() == 1 {
		cmd.id = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return secretCommand{}, fmt.Errorf("secret %s: unexpected argument %q\n%s", cmd.name, fs.Arg(0), secretUsage)
	}
	switch {
	case cmd.name == "rotate" && cmd.all == (cmd.scope != ""):
		return secretCommand{}, fmt.Errorf("secret rotate: give either --scope or --all\n%s", secretUsage)
	case cmd.name == "rewrap" && cmd.to == "":
		return secretCommand{}, fmt.Errorf("secret rewrap: --to <kek id> is required (the KEK ids are listed by relay secret status)\n%s", secretUsage)
	case cmd.name == "destroy" && cmd.id == "":
		return secretCommand{}, fmt.Errorf("secret destroy: the data key id is required\n%s", secretUsage)
	}
	return cmd, nil
}
