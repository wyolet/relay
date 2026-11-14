package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/apply"
)

const teamManifest = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  displayName: Platform
spec:
  enabled: true
`

func manifestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// controlRequest is what a fake control API saw of one request.
type controlRequest struct {
	Method, Path, Query, Auth, ContentType, Cookie, Body string
}

// controlServer answers every request with one status and body and records what it received.
type controlServer struct {
	mu   sync.Mutex
	reqs []controlRequest
	srv  *httptest.Server
}

func newControlServer(t *testing.T, status int, reply string) *controlServer {
	t.Helper()
	c := &controlServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, controlRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Cookie"), string(body)})
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *controlServer) requests() []controlRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]controlRequest(nil), c.reqs...)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// exitCode is the process exit status runCLI gives err.
func exitCode(err error) int {
	var ee *exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	}
	return 1
}

func planReply(t *testing.T, applied bool, counts apply.Counts, plan ...apply.Entry) string {
	t.Helper()
	raw, err := json.Marshal(applyResponse{Plan: plan, Applied: applied, Counts: counts})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func entry(name string, action apply.Action, changed ...string) apply.Entry {
	return apply.Entry{Kind: "Team", Name: name, Action: action, ChangedFields: changed}
}

// The flags reach the server as query parameters, and the plan decides the exit status: 1 when a row was left out, 2 when the server refused. A dropped --dry-run would write, so the query is checked exactly.
func TestRunApply(t *testing.T) {
	create := planReply(t, true, apply.Counts{Create: 1}, entry("platform", apply.ActionCreate))
	const createTable = "KIND  NAME      ACTION  CHANGED\nTeam  platform  create  \n"
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
		{name: "no flags", reply: create, stdout: createTable + "\ncreate=1 update=0 unchanged=0 skip-dirty=0 delete=0 conflict=0 forbidden=0 applied=true\n"},
		{name: "dry run", flags: []string{"--dry-run"}, query: "dryRun=true",
			reply:  planReply(t, false, apply.Counts{Update: 1, Unchanged: 2, Delete: 3}, entry("platform", apply.ActionUpdate, "spec.enabled", "metadata.displayName")),
			stdout: "KIND  NAME      ACTION  CHANGED\nTeam  platform  update  spec.enabled,metadata.displayName\n\ncreate=0 update=1 unchanged=2 skip-dirty=0 delete=3 conflict=0 forbidden=0 applied=false\n"},
		{name: "force, prune and selector", flags: []string{"--force", "--prune", "--selector", "env=prod,team=platform"}, reply: create,
			query: "force=true&prune=true&selector=env%3Dprod%2Cteam%3Dplatform", stdout: createTable + "\ncreate=1 update=0 unchanged=0 skip-dirty=0 delete=0 conflict=0 forbidden=0 applied=true\n"},
		// Only the skip-dirty row differs; an unchanged one is not drift.
		{name: "operator-edited row left alone", code: exitDrift,
			reply: planReply(t, true, apply.Counts{Unchanged: 1, SkipDirty: 1}, entry("platform", apply.ActionSkipDirty), entry("research", apply.ActionUnchanged)),
			err:   "apply: 1 row(s) differ from the manifest and were left alone; re-run with --force to overwrite"},
		{name: "force makes an operator-edited row no error", flags: []string{"--force"}, query: "force=true",
			reply: planReply(t, true, apply.Counts{SkipDirty: 1}, entry("platform", apply.ActionSkipDirty))},
		// --force overrides operator edits, not a row that changed mid-apply or one out of scope.
		{name: "conflict", flags: []string{"--force"}, query: "force=true", code: exitDrift,
			reply: planReply(t, true, apply.Counts{Conflict: 2}, entry("a", apply.ActionConflict), entry("b", apply.ActionConflict), entry("c", apply.ActionSkipDirty)),
			err:   "apply: 2 row(s) changed between plan and write and were skipped; re-run to converge"},
		{name: "forbidden rows outrank conflicts", flags: []string{"--force"}, query: "force=true", code: exitDrift,
			reply: planReply(t, true, apply.Counts{Create: 1, Forbidden: 1}, entry("platform", apply.ActionCreate), entry("other", apply.ActionForbidden), entry("c", apply.ActionConflict)),
			err:   "apply: 1 row(s) are outside your scope and were ignored"},
		{name: "server refuses", status: http.StatusForbidden, reply: `{"error":{"message":"forbidden"}}` + "\n", code: exitFailed,
			err: `server returned 403: {"error":{"message":"forbidden"}}`},
		{name: "reply that is not JSON", reply: "<html>", code: exitFailed, err: "apply: decode response: invalid character '<' looking for beginning of value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withEnvFileState(t)
			t.Setenv("RELAY_ADMIN_TOKEN", "")
			if tc.status == 0 {
				tc.status = http.StatusOK
			}
			srv := newControlServer(t, tc.status, tc.reply)
			dir := manifestDir(t)
			var err error
			out := captureStdout(t, func() {
				err = runApply(append([]string{"-f", dir, "--url", srv.srv.URL + "/", "--token", "tok"}, tc.flags...))
			})
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if exitCode(err) != tc.code || msg != tc.err {
				t.Errorf("exit %d %q, want %d %q", exitCode(err), msg, tc.code, tc.err)
			}
			if tc.stdout != "" && out != tc.stdout {
				t.Errorf("stdout:\n%q\nwant:\n%q", out, tc.stdout)
			}
			if tc.code == exitFailed && out != "" {
				t.Errorf("stdout = %q for a failed apply, want nothing", out)
			}
			bundle, err := loadBundle(dir)
			if err != nil || !strings.Contains(string(bundle), "kind: Team") {
				t.Fatalf("loadBundle = %q, %v", bundle, err)
			}
			want := []controlRequest{{Method: "POST", Path: "/api/apply", Query: tc.query, Auth: "Bearer tok", ContentType: "application/yaml", Body: string(bundle)}}
			if got := srv.requests(); len(got) != 1 || got[0] != want[0] {
				t.Errorf("requests = %+v\n    want %+v", got, want)
			}
		})
	}
}

// Problems on this side are reported before any request is made.
func TestRunApply_RefusesBeforeCallingTheServer(t *testing.T) {
	srv := newControlServer(t, http.StatusOK, `{}`)
	empty := t.TempDir()
	notManifest := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(notManifest, []byte("kind: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string // exact, or a prefix when it ends in "…"
	}{
		{"no -f", []string{"--url", srv.srv.URL}, "apply: -f <file|dir> is required"},
		{"path that does not exist", []string{"-f", filepath.Join(empty, "absent"), "--url", srv.srv.URL}, "stat …"},
		{"directory without manifests", []string{"-f", empty, "--url", srv.srv.URL}, "apply: no manifest documents found in " + empty},
		{"file that does not parse", []string{"-f", notManifest, "--url", srv.srv.URL}, "…"},
		{"unknown flag", []string{"-f", empty, "--yolo"}, "flag provided but not defined: -yolo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withEnvFileState(t)
			err := runApply(tc.args)
			if err == nil || exitCode(err) != 1 {
				t.Fatalf("runApply = %v (exit %d), want a plain error", err, exitCode(err))
			}
			if prefix, ok := strings.CutSuffix(tc.want, "…"); ok && strings.HasPrefix(err.Error(), prefix) {
				return
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q, want %q", err, tc.want)
			}
		})
	}
	if got := srv.requests(); len(got) != 0 {
		t.Errorf("%d requests reached the server", len(got))
	}
}

// A token that came from a dotenv file may belong to another deployment than the default URL, so apply wants the target named.
func TestRunApply_TargetAndTokenSources(t *testing.T) {
	withEnvFileState(t)
	srv := newControlServer(t, http.StatusOK, planReply(t, false, apply.Counts{}))
	dir := manifestDir(t)
	t.Setenv("RELAY_URL", srv.srv.URL)
	t.Setenv("RELAY_ADMIN_TOKEN", "from-file")
	dotEnvFile, dotEnvKeys = "prod.env", map[string]bool{"RELAY_ADMIN_TOKEN": true}

	err := runApply([]string{"-f", dir, "--dry-run"})
	if err == nil || err.Error() != "apply: RELAY_ADMIN_TOKEN comes from prod.env; pass --url to confirm the target" || len(srv.requests()) != 0 {
		t.Fatalf("dotenv token without --url: err = %v after %d requests", err, len(srv.requests()))
	}

	// An explicit --url or --token is the confirmation; an exported token never needed one.
	for name, args := range map[string][]string{"--url": {"--url", srv.srv.URL}, "--token": {"--token", "from-file"}} {
		captureStdout(t, func() { err = runApply(append([]string{"-f", dir}, args...)) })
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	dotEnvKeys = map[string]bool{"RELAY_URL": true}
	captureStdout(t, func() { err = runApply([]string{"-f", dir}) })
	if err != nil {
		t.Errorf("exported token: %v", err)
	}
	got := srv.requests()
	if len(got) != 3 || got[0].Auth != "Bearer from-file" || got[1].Auth != "Bearer from-file" || got[2].Auth != "Bearer from-file" {
		t.Errorf("requests = %+v, want three carrying the environment's token", got)
	}

	for _, tc := range []struct {
		flag, env string
		keys      map[string]bool
		want      string
	}{
		{"http://flag", "http://env", nil, "--url"},
		{"", "", nil, "default"},
		{"", "http://env", nil, "RELAY_URL"},
		{"", "http://env", map[string]bool{"RELAY_URL": true}, "RELAY_URL from prod.env"},
	} {
		t.Setenv("RELAY_URL", tc.env)
		dotEnvKeys = tc.keys
		if got := applyTargetSource(tc.flag); got != tc.want {
			t.Errorf("applyTargetSource(%q) with RELAY_URL=%q = %q, want %q", tc.flag, tc.env, got, tc.want)
		}
	}
}
