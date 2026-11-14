package payload

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s, dir
}

// regularFiles lists every regular file under dir, relative to it.
func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestNewFileStore(t *testing.T) {
	if s, err := NewFileStore(""); err == nil || err.Error() != "payload: empty file store root" || s != nil {
		t.Errorf("NewFileStore(\"\") = %v, %v", s, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := NewFileStore(filepath.Join(file, "sub")); err == nil || !strings.HasPrefix(err.Error(), "payload: create root: ") || s != nil {
		t.Errorf("NewFileStore under a file = %v, %v", s, err)
	}
}

func TestFileStore_RoundTrip(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()

	uri, err := s.Put(ctx, "job-1/input", []byte("hello payload"))
	if err != nil || uri != "file:job-1/input" {
		t.Fatalf("Put = %q, %v", uri, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "job-1", "input"))
	if err != nil || string(onDisk) != "hello payload" {
		t.Fatalf("file on disk = %q, %v", onDisk, err)
	}
	if got, err := s.Get(ctx, uri); err != nil || string(got) != "hello payload" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if uri2, err := s.Put(ctx, "job-1/./input", nil); err != nil || uri2 != "file:job-1/./input" {
		t.Fatalf("Put of an empty payload = %q, %v", uri2, err)
	}
	if got, err := s.Get(ctx, uri); err != nil || len(got) != 0 {
		t.Fatalf("Get after overwriting with an empty payload = %q, %v", got, err)
	}

	if err := s.Delete(ctx, uri); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := s.Get(ctx, uri); !errors.Is(err, fs.ErrNotExist) || got != nil {
		t.Fatalf("Get after Delete = %q, %v; want a not-exist error", got, err)
	}
	if err := s.Delete(ctx, uri); err != nil {
		t.Fatalf("Delete of a missing blob: %v", err)
	}
	if files := regularFiles(t, dir); len(files) != 0 {
		t.Errorf("files left behind: %v", files)
	}
}

// Put must replace via rename: a reader holding the old file keeps a complete old payload.
func TestFileStore_PutReplacesAtomically(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "job/result", []byte("previous-complete-payload")); err != nil {
		t.Fatal(err)
	}
	final, link := filepath.Join(dir, "job", "result"), filepath.Join(dir, "job", "previous-link")
	if err := os.Link(final, link); err != nil {
		t.Skipf("hard links unsupported on temp filesystem: %v", err)
	}
	want := strings.Repeat("complete-payload-", 1024)
	if _, err := s.Put(ctx, "job/result", []byte(want)); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != want {
		t.Fatalf("final file has %d bytes (%v), want %d", len(got), err, len(want))
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "previous-complete-payload" {
		t.Fatalf("hard link to the old file = %q, %v; the old file was rewritten in place", got, err)
	}
	if info, err := os.Stat(final); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("final file mode = %v, %v; want 0644", info.Mode().Perm(), err)
	}
	if files := regularFiles(t, dir); len(files) != 2 {
		t.Errorf("files after Put = %v, want only the blob and the link", files)
	}
}

func TestFileStore_Rejects(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	// A sibling whose name starts with the root's: a prefix check on the raw path would let it through.
	sibling := "../" + filepath.Base(dir) + "-sibling/x"
	if _, err := s.Put(ctx, "kept", []byte("x")); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"../escape", "a/../../escape", "..", sibling} {
		want := `payload: key escapes root: "` + key + `"`
		if uri, err := s.Put(ctx, key, []byte("x")); err == nil || err.Error() != want || uri != "" {
			t.Errorf("Put(%q) = %q, %v; want %q", key, uri, err, want)
		}
		if _, err := s.Get(ctx, "file:"+key); err == nil || err.Error() != want {
			t.Errorf("Get(file:%s) = %v, want %q", key, err, want)
		}
		if err := s.Delete(ctx, "file:"+key); err == nil || err.Error() != want {
			t.Errorf("Delete(file:%s) = %v, want %q", key, err, want)
		}
	}
	for _, uri := range []string{"s3://bucket/key", "kept", "", "FILE:kept"} {
		want := `payload: not a file URI: "` + uri + `"`
		if _, err := s.Get(ctx, uri); err == nil || err.Error() != want {
			t.Errorf("Get(%q) = %v, want %q", uri, err, want)
		}
		if err := s.Delete(ctx, uri); err == nil || err.Error() != want {
			t.Errorf("Delete(%q) = %v, want %q", uri, err, want)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Put(cancelled, "late", []byte("x")); err != context.Canceled {
		t.Errorf("Put on a cancelled context = %v", err)
	}
	if _, err := s.Get(cancelled, "file:kept"); err != context.Canceled {
		t.Errorf("Get on a cancelled context = %v", err)
	}

	parent := filepath.Dir(dir)
	if files := regularFiles(t, parent); len(files) != 1 || files[0] != "root/kept" {
		t.Errorf("files under %s = %v, want only root/kept", parent, files)
	}
}
