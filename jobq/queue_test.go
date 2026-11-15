package jobq

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/jobq/payload"
)

// recordingPayloads is a payload.Store over a map that keeps the order of its calls.
type recordingPayloads struct {
	blobs map[string]string
	calls []string
	err   error
}

func (s *recordingPayloads) Put(_ context.Context, key string, data []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.blobs["mem:"+key] = string(data)
	s.calls = append(s.calls, "put "+string(data))
	return "mem:" + key, nil
}

func (s *recordingPayloads) Get(_ context.Context, uri string) ([]byte, error) {
	return []byte(s.blobs[uri]), nil
}

func (s *recordingPayloads) Delete(_ context.Context, uri string) error {
	s.calls = append(s.calls, "delete "+s.blobs[uri])
	delete(s.blobs, uri)
	return nil
}

var _ payload.Store = (*recordingPayloads)(nil)

// closedPool is a pool every query fails on.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://relay_invalid:relay_invalid@127.0.0.1:1/relay_invalid")
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	pool.Close()
	return pool
}

func TestEnqueue_FailedInsertDeletesTheStoredInput(t *testing.T) {
	ps := &recordingPayloads{blobs: map[string]string{}}
	q := New(closedPool(t), ps, Options{})
	ctx := context.Background()

	if id, err := q.Enqueue(ctx, []byte("orphan me not"), EnqueueOpts{Queue: "default"}); err == nil || id != "" {
		t.Fatalf("Enqueue on a closed pool = %q, %v", id, err)
	}
	if len(ps.blobs) != 0 || len(ps.calls) != 2 || ps.calls[0] != "put orphan me not" || ps.calls[1] != "delete orphan me not" {
		t.Errorf("payload calls = %v leaving %v, want the input stored then deleted", ps.calls, ps.blobs)
	}

	// No input, nothing to store or delete.
	ps.calls = nil
	if _, err := q.Enqueue(ctx, nil, EnqueueOpts{}); err == nil || len(ps.calls) != 0 {
		t.Errorf("Enqueue without input = %v with payload calls %v", err, ps.calls)
	}

	down := errors.New("payload store down")
	ps.err = down
	if id, err := q.Enqueue(ctx, []byte("x"), EnqueueOpts{}); !errors.Is(err, down) || err.Error() != "jobq: store input: payload store down" || id != "" {
		t.Errorf("Enqueue with the payload store down = %q, %v", id, err)
	}
}

func TestQueue_StartNeedsAHandler(t *testing.T) {
	q := New(closedPool(t), &recordingPayloads{}, Options{Concurrency: 3})
	if err := q.Start(context.Background()); err == nil || err.Error() != "jobq: no handlers registered" {
		t.Fatalf("Start = %v", err)
	}
	// A refused Start starts nothing, so Wait returns.
	q.Wait()

	if inUse, limit := q.Concurrency(); inUse != 0 || limit != 3 {
		t.Errorf("Concurrency = %d of %d, want 0 of 3", inUse, limit)
	}
	q.SetConcurrency(5)
	if _, limit := q.Concurrency(); limit != 5 {
		t.Errorf("limit after SetConcurrency(5) = %d", limit)
	}
}
