//go:build integration

package integration_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// slowTestsEnv opts into cases that wait on real timers for 20 s or more.
const slowTestsEnv = "RELAY_TEST_SLOW"

// requireSlow skips t unless slowTestsEnv is "1".
func requireSlow(t *testing.T, why string) {
	t.Helper()
	if os.Getenv(slowTestsEnv) != "1" {
		t.Skipf("%s; set %s=1 to run it", why, slowTestsEnv)
	}
}

// relayBinary builds cmd/relay into the test's temp dir.
func relayBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "relay")
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(file))
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/relay")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build relay: %v\n%s", err, out)
	}
	return bin
}

// relayProc is one relay process on its own pair of loopback ports.
type relayProc struct {
	t          *testing.T
	name       string
	cmd        *exec.Cmd
	infer      string
	control    string
	adminToken string
	logs       *lockedBuffer
	exited     chan struct{}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// relayEnv is the environment every relay in the suite boots with: the given
// database, the catalog served by w, nothing inherited from a deployment.
func relayEnv(w *catalogWorld, dsn string) map[string]string {
	return map[string]string{
		"RELAY_PG_DSN":            dsn,
		"RELAY_CATALOG_INDEX_URL": w.http.URL + "/index.yaml",
		"RELAY_CATALOG_URL":       w.http.URL + "/releases/{version}/relay-catalog-data-{version}.tar.gz",
		"RELAY_COOKIE_SECURE":     "false",
		"RELAY_UI_DISABLE":        "1",
		"RELAY_LOG_LEVEL":         "debug",
	}
}

// startRelay boots the relay binary with env (RELAY_PORT, RELAY_CONTROL_PORT,
// the admin token and the master key are filled in) and waits for both
// listeners. A "{infer}" in any value becomes the inference base URL.
func startRelay(t *testing.T, bin, name string, env map[string]string) *relayProc {
	t.Helper()
	inferPort, ctrlPort := freePort(t), freePort(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	p := &relayProc{
		t: t, name: name,
		infer:      fmt.Sprintf("http://127.0.0.1:%d", inferPort),
		control:    fmt.Sprintf("http://127.0.0.1:%d", ctrlPort),
		adminToken: base64.RawURLEncoding.EncodeToString(tok),
		logs:       &lockedBuffer{},
		exited:     make(chan struct{}),
	}
	cfgDir := t.TempDir()
	vars := map[string]string{
		"RELAY_PORT":         strconv.Itoa(inferPort),
		"RELAY_CONTROL_PORT": strconv.Itoa(ctrlPort),
		"RELAY_ADMIN_TOKEN":  p.adminToken,
		"RELAY_MASTER_KEY":   base64.StdEncoding.EncodeToString(key),
		"RELAY_CONFIG_DIR":   cfgDir,
		"RELAY_EVENTLOG_DIR": t.TempDir(),
	}
	for k, v := range env {
		vars[k] = strings.ReplaceAll(v, "{infer}", p.infer)
	}
	cmd := exec.Command(bin)
	cmd.Dir = cfgDir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RELAY_") && !strings.HasPrefix(kv, "WYOLET_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.cmd = cmd
	go func() { _ = cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("relay %s logs:\n%s", name, tail(p.logs.String(), 60))
		}
	})
	waitFor(t, 60*time.Second, "relay "+name+" listening", func() bool {
		select {
		case <-p.exited:
			t.Fatalf("relay %s exited at boot:\n%s", name, tail(p.logs.String(), 60))
		default:
		}
		for _, u := range []string{p.infer + "/livez", p.control + "/api/livez"} {
			res, err := http.Get(u)
			if err != nil {
				return false
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return false
			}
		}
		return true
	})
	return p
}

func (p *relayProc) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.exited:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

func (p *relayProc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// do sends a request to the control listener, with token as a bearer
// credential when non-empty.
func (p *relayProc) do(method, path string, body io.Reader, token string) (int, []byte) {
	p.t.Helper()
	req, err := http.NewRequest(method, p.control+path, body)
	if err != nil {
		p.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 90 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// marker is the catalog-source settings section.
type marker struct {
	Version   string `json:"version"`
	SeededAt  string `json:"seededAt"`
	CheckedAt string `json:"checkedAt"`
	LastError string `json:"lastError"`
}

func (p *relayProc) marker() marker {
	p.t.Helper()
	code, body := p.do(http.MethodGet, "/api/settings/catalog-source", nil, p.adminToken)
	if code != http.StatusOK {
		p.t.Fatalf("GET /api/settings/catalog-source = %d: %s", code, body)
	}
	var m marker
	if err := json.Unmarshal(body, &m); err != nil {
		p.t.Fatalf("catalog-source: %v: %s", err, body)
	}
	if m == (marker{}) {
		// The section may come wrapped in an envelope.
		var wrapped map[string]json.RawMessage
		_ = json.Unmarshal(body, &wrapped)
		for _, k := range []string{"value", "spec", "data"} {
			if raw, ok := wrapped[k]; ok {
				_ = json.Unmarshal(raw, &m)
			}
		}
	}
	return m
}

// waitMarker waits until the marker names version.
func (p *relayProc) waitMarker(version string, within time.Duration) marker {
	p.t.Helper()
	var m marker
	waitFor(p.t, within, "relay "+p.name+" catalog-source version "+version, func() bool {
		m = p.marker()
		return m.Version == version
	})
	return m
}

// waitFollowing waits until the release hook answers, which it does once
// the follower is mounted.
func (p *relayProc) waitFollowing() {
	p.t.Helper()
	waitFor(p.t, 30*time.Second, "relay "+p.name+" release hook", func() bool {
		code, _ := p.nudge(`{"type":"challenge","challenge":"00000000000000000000000000000000"}`)
		return code == http.StatusOK
	})
}

// hasModel reports whether the catalog read API knows the model.
func (p *relayProc) hasModel(name string) bool {
	code, _ := p.do(http.MethodGet, "/api/models/"+name, nil, p.adminToken)
	return code == http.StatusOK
}

func (p *relayProc) metric(name string, labels ...string) float64 {
	return scrapeMetric(p.t, p.control+"/metrics", p.adminToken, name, labels...)
}

// checks sums relay_catalog_checks_total over every result.
func (p *relayProc) checks() float64 { return p.metric("relay_catalog_checks_total") }

// nudge posts body to the relay's release webhook receiver.
func (p *relayProc) nudge(body string) (int, []byte) {
	p.t.Helper()
	res, err := http.Post(p.infer+"/catalog/hooks/release", "application/json", strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// scrapeMetric sums every sample of name whose labels include all of labels
// (given as k=v pairs).
func scrapeMetric(t *testing.T, url, token, name string, labels ...string) float64 {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer res.Body.Close()
	var sum float64
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		if rest == "" || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		ok := true
		for _, l := range labels {
			k, v, _ := strings.Cut(l, "=")
			if !strings.Contains(rest, k+`="`+v+`"`) {
				ok = false
			}
		}
		if !ok {
			continue
		}
		fields := strings.Fields(rest[strings.LastIndex(rest, "}")+1:])
		if len(fields) == 0 {
			continue
		}
		if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
			sum += f
		}
	}
	return sum
}

func metricString(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
