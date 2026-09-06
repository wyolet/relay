package role

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/internal/storage/gen"
)

// refusingDB records whether the store was reached and fails every query, so
// the seed stops at its first read.
type refusingDB struct{ queried bool }

func (d *refusingDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("refusingDB")
}

func (d *refusingDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	d.queried = true
	return nil, errors.New("refusingDB")
}

func (d *refusingDB) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

func (d *refusingDB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("refusingDB")
}

// Pods booting together must not each mint an id for the same missing role,
// so every read and write of the seed runs inside the caller's lock.
func TestSeedBuiltinsRunsInsideTheLock(t *testing.T) {
	db := &refusingDB{}
	s := NewStore(gen.New(db))

	locked, unlocked := false, false
	lock := func(ctx context.Context, fn func(context.Context) error) error {
		locked = true
		if db.queried {
			t.Error("the store was read before the lock was taken")
		}
		err := fn(ctx)
		unlocked = true
		return err
	}

	if err := SeedBuiltins(context.Background(), s, nil, lock); err == nil {
		t.Fatal("SeedBuiltins reported success against a failing store")
	}
	if !locked {
		t.Fatal("the seed ran without taking the lock")
	}
	if !db.queried {
		t.Error("the seed never reached the store")
	}
	if !unlocked {
		t.Error("the lock was never released")
	}
}
