package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadYAML_EnvAndFile(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw.txt")
	if err := os.WriteFile(pwFile, []byte("hunter2hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_USERNAME", "alice")

	writeFile(t, dir, "admin.yaml", `apiVersion: relay.wyolet.dev/v1
kind: User
metadata:
  name: admin
spec:
  username:
    valueFrom: {env: RELAY_TEST_USERNAME}
  email:
    value: admin@example.com
  password:
    valueFrom:
      file: `+pwFile+`
  roles: [admin]
`)

	store, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	u, ok := store.ByName("admin")
	if !ok {
		t.Fatal("admin not found")
	}
	if u.Spec.Username.Get() != "alice" {
		t.Errorf("username=%q want alice", u.Spec.Username.Get())
	}
	if u.Spec.Email.Get() != "admin@example.com" {
		t.Errorf("email=%q", u.Spec.Email.Get())
	}
	if u.Spec.Password.Get() != "hunter2hunter2" {
		t.Errorf("password=%q (trailing newline not trimmed?)", u.Spec.Password.Get())
	}
	if u.Spec.Username.Source() != "env:RELAY_TEST_USERNAME" {
		t.Errorf("source=%q", u.Spec.Username.Source())
	}
}

func TestLoadYAML_BothValueAndValueFromRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "u.yaml", `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: bad}
spec:
  username: {value: x, valueFrom: {env: FOO}}
  email: {value: x@y.com}
  password: {value: longenoughpw}
`)
	if _, err := LoadYAML(dir); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoadYAML_MissingEnvFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "u.yaml", `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: bad}
spec:
  username: {valueFrom: {env: RELAY_DEFINITELY_NOT_SET_XYZ}}
  email: {value: x@y.com}
  password: {value: longenoughpw}
`)
	if _, err := LoadYAML(dir); err == nil {
		t.Fatal("expected error for missing env")
	}
}

func TestLoadYAML_IgnoresOtherKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata: {name: openai}
spec: {kind: openai, baseURL: https://x}
`)
	store, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(store.Users()) != 0 {
		t.Fatalf("expected 0 users, got %d", len(store.Users()))
	}
}

func TestLoadYAML_ScalarShorthand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_TEST_PW", "longenoughpw")
	writeFile(t, dir, "u.yaml", `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: admin}
spec:
  username: admin
  email: admin@example.com
  password:
    valueFrom: {env: RELAY_TEST_PW}
`)
	store, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	u, _ := store.ByName("admin")
	if u.Spec.Username.Get() != "admin" || u.Spec.Username.Source() != "literal" {
		t.Errorf("username=%q source=%q", u.Spec.Username.Get(), u.Spec.Username.Source())
	}
	if u.Spec.Email.Get() != "admin@example.com" {
		t.Errorf("email=%q", u.Spec.Email.Get())
	}
	if u.Spec.Password.Get() != "longenoughpw" {
		t.Errorf("password=%q", u.Spec.Password.Get())
	}
}

func TestLoadYAML_BadEmail(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "u.yaml", `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: bad}
spec:
  username: {value: alice}
  email:    {value: not-an-email}
  password: {value: longenoughpw}
`)
	if _, err := LoadYAML(dir); err == nil {
		t.Fatal("expected error for bad email")
	}
}
