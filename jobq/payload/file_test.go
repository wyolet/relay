package payload

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStore_RoundTrip(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	want := []byte("hello payload")
	uri, err := s.Put(ctx, "job-1/input", want)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, uri)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	if err := s.Delete(ctx, uri); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, uri); err == nil {
		t.Fatal("get after delete returned nil error")
	}
	// Deleting a missing blob is not an error.
	if err := s.Delete(ctx, uri); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestFileStore_PutLeavesCompleteFileAndNoTemps(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	old := []byte("previous-complete-payload")
	uri, err := s.Put(ctx, "job-atomic/result", old)
	if err != nil {
		t.Fatalf("initial put: %v", err)
	}
	finalPath := filepath.Join(dir, "job-atomic", "result")
	hardLinkPath := filepath.Join(dir, "job-atomic", "previous-link")
	if err := os.Link(finalPath, hardLinkPath); err != nil {
		t.Skipf("hard links unsupported on temp filesystem: %v", err)
	}

	want := []byte(strings.Repeat("complete-payload-", 1024))
	uri, err = s.Put(ctx, "job-atomic/result", want)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, uri)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get returned %d bytes, want %d complete bytes", len(got), len(want))
	}

	onDisk, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(onDisk) != string(want) {
		t.Fatalf("final file has %d bytes, want %d complete bytes", len(onDisk), len(want))
	}
	linked, err := os.ReadFile(hardLinkPath)
	if err != nil {
		t.Fatalf("read hard link: %v", err)
	}
	if string(linked) != string(old) {
		t.Fatalf("hard link content = %q, want original content; Put must replace via rename, not rewrite in place", linked)
	}

	var temps []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && strings.Contains(d.Name(), ".tmp-") {
			temps = append(temps, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk payload dir: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary payload files left after Put: %v", temps)
	}
}

func TestFileStore_RejectsTraversal(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := s.Put(context.Background(), "../escape", []byte("x")); err == nil {
		t.Fatal("put with traversal key returned nil error")
	}
}

func TestFileStore_RejectsForeignURI(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := s.Get(context.Background(), "s3://bucket/key"); err == nil {
		t.Fatal("get with non-file URI returned nil error")
	}
}
