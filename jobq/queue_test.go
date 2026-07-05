package jobq

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/jobq/payload"
)

func TestEnqueueDeletesInputPayloadWhenInsertFails(t *testing.T) {
	ctx := context.Background()
	payloadDir := t.TempDir()
	ps, err := payload.NewFileStore(payloadDir)
	if err != nil {
		t.Fatalf("payload store: %v", err)
	}
	pool, err := pgxpool.New(ctx, "postgres://relay_invalid:relay_invalid@127.0.0.1:1/relay_invalid")
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	pool.Close()
	q := New(pool, ps, Options{})

	if _, err := q.Enqueue(ctx, []byte("orphan me not"), EnqueueOpts{Queue: "default"}); err == nil {
		t.Fatal("Enqueue with a closed store returned nil error")
	}

	var files []string
	if err := filepath.WalkDir(payloadDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk payload dir: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("payload files left after failed enqueue: %v", files)
	}
}
