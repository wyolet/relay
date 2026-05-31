package payload

import (
	"context"
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
