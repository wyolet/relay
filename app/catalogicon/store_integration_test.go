//go:build integration

package catalogicon_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalogicon"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

func TestStore_ReplaceIsTheWholeSet(t *testing.T) {
	ctx := context.Background()
	store := catalogicon.NewStore(storagetest.Pool(t))
	claude, kimi := catalogicon.New("claude.png", []byte("one")), catalogicon.New("kimi.png", []byte("two"))

	if err := store.Replace(ctx, []catalogicon.Icon{claude, kimi}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "claude.png")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(claude, got); diff != "" {
		t.Errorf("claude.png (-want +got):\n%s", diff)
	}

	redrawn := catalogicon.New("claude.png", []byte("three"))
	if err := store.Replace(ctx, []catalogicon.Icon{redrawn}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, "claude.png"); err != nil || got.SHA256 != redrawn.SHA256 {
		t.Errorf("replaced icon: %+v, err %v", got, err)
	}
	if _, err := store.Get(ctx, "kimi.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dropped icon: err %v, want fs.ErrNotExist", err)
	}
}
