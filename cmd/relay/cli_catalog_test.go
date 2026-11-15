package main

import (
	"net/http"
	"testing"
)

const pruneReply = `{"dryRun":true,"idle":"720h0m0s","pruned":1,"keptDetached":1,"rows":[` +
	`{"kind":"host-binding","name":"old-chat-on-acme","id":"b1","detachedAt":"2026-11-01T00:00:00Z","decision":"prune"},` +
	`{"kind":"model","name":"busy-chat","id":"m1","detachedAt":"2026-11-01T00:00:00Z","decision":"keep","reason":"traffic since 2026-10-16T12:00:00Z (last request 2026-11-15T10:00:00Z)"}]}`

const pruneTable = "KIND          NAME              DECISION  REASON\n" +
	"host-binding  old-chat-on-acme  prune     \n" +
	"model         busy-chat         keep      traffic since 2026-10-16T12:00:00Z (last request 2026-11-15T10:00:00Z)\n"

// A prune is a dry run unless --apply says otherwise; the query is checked
// exactly, since a dropped dryRun=true would delete.
func TestRunCatalogPrune(t *testing.T) {
	tests := []struct {
		name   string
		flags  []string
		status int
		reply  string
		query  string
		code   int
		err    string
		stdout string
	}{
		{name: "dry run by default", query: "dryRun=true", reply: pruneReply,
			stdout: pruneTable + "\npruned=1 kept-detached=1 idle=720h0m0s applied=false\n"},
		{name: "apply with a window", flags: []string{"--apply", "--idle", "30d"}, query: "dryRun=false&idle=30d",
			reply:  `{"dryRun":false,"idle":"720h0m0s","pruned":0,"keptDetached":0,"rows":[]}`,
			stdout: "KIND  NAME  DECISION  REASON\n\npruned=0 kept-detached=0 idle=720h0m0s applied=true\n"},
		{name: "server refuses", status: http.StatusForbidden, query: "dryRun=true", reply: `{"error":{"message":"forbidden"}}`,
			code: exitFailed, err: `server returned 403: {"error":{"message":"forbidden"}}`},
		{name: "another seeding", flags: []string{"--apply"}, status: http.StatusServiceUnavailable, query: "dryRun=false", reply: `{"error":{"message":"busy"}}`,
			code: exitFailed, err: `server returned 503: {"error":{"message":"busy"}}`},
		{name: "reply that is not JSON", query: "dryRun=true", reply: "<html>", code: exitFailed,
			err: "catalog prune: decode response: invalid character '<' looking for beginning of value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnvFileState(t)
			if tc.status == 0 {
				tc.status = http.StatusOK
			}
			srv := newControlServer(t, tc.status, tc.reply)
			var err error
			out := captureStdout(t, func() {
				err = runCatalog(append([]string{"prune", "--url", srv.srv.URL + "/", "--token", "tok"}, tc.flags...))
			})
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if exitCode(err) != tc.code || msg != tc.err || out != tc.stdout {
				t.Errorf("exit %d %q stdout %q; want %d %q %q", exitCode(err), msg, out, tc.code, tc.err, tc.stdout)
			}
			want := controlRequest{Method: "POST", Path: "/api/catalog/prune", Query: tc.query, Auth: "Bearer tok"}
			if got := srv.requests(); len(got) != 1 || got[0] != want {
				t.Errorf("requests = %+v\n    want %+v", got, want)
			}
		})
	}
}

// Usage mistakes, and an apply aimed by a dotenv token alone, stop before any request.
func TestRunCatalogPrune_RefusesBeforeCallingTheServer(t *testing.T) {
	srv := newControlServer(t, http.StatusOK, pruneReply)
	for _, tc := range []struct {
		name    string
		args    []string
		fromEnv bool
		want    string
	}{
		{"no verb", nil, false, catalogUsage},
		{"another verb", []string{"sync"}, false, catalogUsage},
		{"apply with a token from a dotenv file", []string{"prune", "--apply"}, true, "catalog prune: RELAY_ADMIN_TOKEN comes from .env.test; pass --url to confirm the target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withEnvFileState(t)
			t.Setenv("RELAY_URL", srv.srv.URL)
			if tc.fromEnv {
				dotEnvFile, dotEnvKeys = ".env.test", map[string]bool{"RELAY_ADMIN_TOKEN": true}
				t.Setenv("RELAY_ADMIN_TOKEN", "tok")
			}
			if err := runCatalog(tc.args); err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if got := srv.requests(); len(got) != 0 {
		t.Fatalf("requests = %+v, want none", got)
	}
}
