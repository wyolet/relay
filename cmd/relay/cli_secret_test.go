package main

import (
	"strings"
	"testing"
)

func TestParseSecretArgs(t *testing.T) {
	const id = "0192f000-0000-7000-8000-000000000001"
	for _, tc := range []struct {
		args    []string
		want    secretCommand
		wantErr string
	}{
		{args: []string{"status"}, want: secretCommand{name: "status"}},
		{args: []string{"migrate"}, want: secretCommand{name: "migrate"}},
		{args: []string{"rotate", "--scope", "project:p1"}, want: secretCommand{name: "rotate", scope: "project:p1"}},
		{args: []string{"rotate", "--all"}, want: secretCommand{name: "rotate", all: true}},
		{args: []string{"rewrap", "--to", "00112233445566778899aabbccddeeff"}, want: secretCommand{name: "rewrap", to: "00112233445566778899aabbccddeeff"}},
		{args: []string{"destroy", id}, want: secretCommand{name: "destroy", id: id}},
		{args: []string{"destroy", id, "--force"}, want: secretCommand{name: "destroy", id: id, force: true}},
		{args: []string{"destroy", "--force", id}, want: secretCommand{name: "destroy", id: id, force: true}},

		{args: nil, wantErr: "usage"},
		{args: []string{"frobnicate"}, wantErr: "usage"},
		{args: []string{"status", "extra"}, wantErr: "unexpected argument"},
		{args: []string{"rotate"}, wantErr: "either --scope or --all"},
		{args: []string{"rotate", "--scope", "system", "--all"}, wantErr: "either --scope or --all"},
		{args: []string{"rewrap"}, wantErr: "--to"},
		{args: []string{"rewrap", "--to"}, wantErr: "flag needs an argument"},
		{args: []string{"destroy"}, wantErr: "data key id is required"},
		{args: []string{"destroy", "--force"}, wantErr: "data key id is required"},
		{args: []string{"destroy", id, "another"}, wantErr: "unexpected argument"},
		{args: []string{"rotate", "--force"}, wantErr: "not defined"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got, err := parseSecretArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// Bad arguments are refused before any configuration or database is needed.
func TestRunSecretRefusesBadArgumentsWithoutADatabase(t *testing.T) {
	t.Setenv("RELAY_PG_DSN", "")
	if err := runSecret([]string{"rotate"}); err == nil || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("runSecret(rotate) = %v", err)
	}
}
