package apply

import (
	"context"
	"testing"
)

// planOne indexes b's rows and plans w's documents through planKind, with
// writes that record the row they would persist instead of storing it.
// written fills as the caller runs the entries' writes; Delete is a no-op.
func planOne[D, T any](ctx context.Context, b *builder, w kindWiring[D, T]) (written *[]*T, err error) {
	written = new([]*T)
	b.idx = newIndex(b.rows)
	w.Upsert = func(_ context.Context, x *T) error {
		*written = append(*written, x)
		return nil
	}
	w.Delete = func(context.Context, string) error { return nil }
	return written, planKind(ctx, b, w)
}

// runWrites runs every write the plan in b holds.
func runWrites(t *testing.T, b *builder) {
	t.Helper()
	for _, e := range b.entries {
		if e.write != nil {
			if err := e.write(context.Background()); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
}
