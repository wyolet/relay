//go:build integration

package batch

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/jobq"
	"github.com/wyolet/relay/jobq/payload"
)

// These tests exercise the batch subsystem's own bookkeeping — Submit, Status,
// Results, Cancel, and owner isolation — over a real jobq queue + Postgres
// Store, using a stub handler in place of the inference Runner (which needs a
// live upstream and is covered by the pipeline/inference tests).

func testService(t *testing.T, handler jobq.Handler) (*Service, *jobq.Queue, context.CancelFunc) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	st, err := storagemod.Open(ctx, dsn) // runs relay migrations incl. 000019_batches
	if err != nil {
		t.Fatalf("storage open: %v", err)
	}
	t.Cleanup(st.Close)
	pool := st.Pool()
	if err := jobq.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobq migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE batches CASCADE; TRUNCATE jobq_jobs"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	ps, err := payload.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("payload store: %v", err)
	}
	q := jobq.New(pool, ps, jobq.Options{
		Concurrency:      4,
		PollInterval:     10 * time.Millisecond,
		ScheduleInterval: 10 * time.Millisecond,
		RescueInterval:   10 * time.Millisecond,
		RescueAfter:      time.Hour,
		JobTimeout:       5 * time.Second,
	})
	q.Register(Queue, handler)
	runCtx, cancel := context.WithCancel(ctx)
	if err := q.Start(runCtx); err != nil {
		t.Fatalf("queue start: %v", err)
	}
	t.Cleanup(func() { cancel(); q.Wait() })
	// runner is nil: these tests drive a stub handler, never Service.Handler().
	return NewService(NewStore(pool), q, nil, nil), q, cancel
}

// keyCaller is a submission by a service-account key: the owner token is the
// key hash, as it has always been.
func keyCaller(hash, policyID string) *Caller {
	c := &Caller{KeyHash: hash, PolicyID: policyID}
	c.PrincipalKind, c.PrincipalID = string(key.PrincipalServiceAccount), "sa-"+hash
	c.CredentialKind, c.CredentialID = "key", "k-"+hash
	return c
}

func waitCounts(t *testing.T, svc *Service, id, hash string, state jobq.State, want int, timeout time.Duration) *BatchView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v, err := svc.Status(context.Background(), id, hash)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if v.Counts[state] >= want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch %s: %s reached %d, want %d", id, state, v.Counts[state], want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIntegration_SubmitRunResults(t *testing.T) {
	echo := func(_ context.Context, job *jobq.Job) ([]byte, error) {
		return append([]byte("echo:"), job.Input()...), nil
	}
	svc, _, _ := testService(t, echo)
	ctx := context.Background()
	const hash = "ownerhash1"

	items := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	id, err := svc.Submit(ctx, keyCaller(hash, "policy1"), "openai", items)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	view := waitCounts(t, svc, id, hash, jobq.StateCompleted, 3, 3*time.Second)
	if view.TotalItems != 3 {
		t.Fatalf("total_items = %d, want 3", view.TotalItems)
	}

	results, err := svc.Results(ctx, id, hash)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for _, r := range results {
		if r.State != jobq.StateCompleted {
			t.Errorf("item %d state = %s, want completed", r.Idx, r.State)
		}
		want := "echo:" + string(items[r.Idx])
		if string(r.Response) != want {
			t.Errorf("item %d response = %q, want %q", r.Idx, r.Response, want)
		}
	}
}

func TestIntegration_OwnerIsolation(t *testing.T) {
	svc, _, _ := testService(t, func(_ context.Context, j *jobq.Job) ([]byte, error) { return j.Input(), nil })
	ctx := context.Background()

	id, err := svc.Submit(ctx, keyCaller("owner", "p"), "openai", [][]byte{[]byte("x")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := svc.Status(ctx, id, "intruder"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("status as intruder: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Results(ctx, id, "intruder"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("results as intruder: err = %v, want ErrForbidden", err)
	}
	if err := svc.Cancel(ctx, id, "intruder"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cancel as intruder: err = %v, want ErrForbidden", err)
	}
}

func TestIntegration_Cancel(t *testing.T) {
	release := make(chan struct{})
	blocking := func(ctx context.Context, _ *jobq.Job) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return []byte("done"), nil
		}
	}
	svc, _, _ := testService(t, blocking)
	ctx := context.Background()
	const hash = "cancelowner"

	id, err := svc.Submit(ctx, keyCaller(hash, "p"), "openai", [][]byte{[]byte("1"), []byte("2")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Wait until at least one item is running, then cancel the batch.
	waitCounts(t, svc, id, hash, jobq.StateRunning, 1, 3*time.Second)
	if err := svc.Cancel(ctx, id, hash); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(release)

	// Batch row is marked cancelled; items settle to cancelled (running ones
	// via ctx-cancel, pending ones directly).
	b, err := svc.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.Status != StatusCancelled {
		t.Fatalf("batch status = %s, want cancelled", b.Status)
	}
	waitCounts(t, svc, id, hash, jobq.StateCancelled, 2, 3*time.Second)
}

// A submission records the principal's tenancy so an item that runs minutes
// later is attributed to who submitted it, not to whatever the credential
// resolves to at execution time.
func TestIntegration_SubmitRecordsAttribution(t *testing.T) {
	var seen Attribution
	// Mirrors Service.Handler: the item reads its attribution off its own job
	// metadata, which is what the runner puts on the lifecycle.
	capture := func(_ context.Context, job *jobq.Job) ([]byte, error) {
		seen = Attribution{
			ProjectID:      job.Meta(metaProjectID),
			TeamID:         job.Meta(metaTeamID),
			PrincipalKind:  job.Meta(metaPrincipalKind),
			PrincipalID:    job.Meta(metaPrincipalID),
			CredentialKind: job.Meta(metaCredentialKind),
			CredentialID:   job.Meta(metaCredentialID),
		}
		return job.Input(), nil
	}
	svc, _, _ := testService(t, capture)
	ctx := context.Background()
	const hash = "attribhash"

	c := &Caller{KeyHash: hash, PolicyID: "policy1"}
	c.ProjectID, c.TeamID = "p1", "t1"
	c.PrincipalKind, c.PrincipalID = string(key.PrincipalServiceAccount), "sa1"
	c.CredentialKind, c.CredentialID = "key", "k1"
	id, err := svc.Submit(ctx, c, "openai", [][]byte{[]byte("a")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	b, err := svc.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.ProjectID != "p1" || b.TeamID != "t1" {
		t.Fatalf("tenancy: %q / %q", b.ProjectID, b.TeamID)
	}
	if b.PrincipalKind != string(key.PrincipalServiceAccount) || b.PrincipalID != "sa1" {
		t.Fatalf("principal: %q / %q", b.PrincipalKind, b.PrincipalID)
	}
	if b.CredentialKind != "key" || b.CredentialID != "k1" {
		t.Fatalf("credential: %q / %q", b.CredentialKind, b.CredentialID)
	}

	waitCounts(t, svc, id, hash, jobq.StateCompleted, 1, 3*time.Second)
	if seen != b.Attribution {
		t.Fatalf("execution read %+v from job metadata, want %+v", seen, b.Attribution)
	}
}

// tokenCaller is a submission by an inference token: it presents no key, so
// the owner token falls back to the principal.
func tokenCaller(userID, jti, policyID string) *Caller {
	c := &Caller{PolicyID: policyID}
	c.ProjectID, c.TeamID = "p1", "t1"
	c.PrincipalKind, c.PrincipalID = string(key.PrincipalUser), userID
	c.CredentialKind, c.CredentialID = "token", jti
	return c
}

// A token authenticates a submission exactly as a key does: rejecting it
// meant tokens could reach /v1/chat/completions but not /v1/batches.
func TestIntegration_SubmitAcceptsATokenCaller(t *testing.T) {
	echo := func(_ context.Context, job *jobq.Job) ([]byte, error) { return job.Input(), nil }
	svc, _, _ := testService(t, echo)
	ctx := context.Background()

	c := tokenCaller("u-alice", "jti-1", "policy1")
	id, err := svc.Submit(ctx, c, "openai", [][]byte{[]byte("a")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	b, err := svc.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.CredentialKind != "token" || b.CredentialID != "jti-1" {
		t.Fatalf("credential: %q / %q", b.CredentialKind, b.CredentialID)
	}
	waitCounts(t, svc, id, c.Owner(), jobq.StateCompleted, 1, 3*time.Second)

	// Two token callers must not reach each other's batches just because
	// neither presents a key hash.
	other := tokenCaller("u-bob", "jti-2", "policy1")
	if _, err := svc.Status(ctx, id, other.Owner()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("status as another token holder: err = %v, want ErrForbidden", err)
	}
}

// The policy recorded is the one the auth layer resolved (key → service
// account → policy binding), not the key row's own field, which is empty for
// a key that inherits its policy.
func TestIntegration_SubmitRecordsTheResolvedPolicy(t *testing.T) {
	echo := func(_ context.Context, job *jobq.Job) ([]byte, error) { return job.Input(), nil }
	svc, _, _ := testService(t, echo)
	ctx := context.Background()

	c := keyCaller("inherited", "bound-policy")
	id, err := svc.Submit(ctx, c, "openai", [][]byte{[]byte("a")})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	b, err := svc.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b.PolicyID != "bound-policy" {
		t.Fatalf("policy = %q, want the resolved one", b.PolicyID)
	}
}
