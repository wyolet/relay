package stored

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
)

func TestParseKeyIDs(t *testing.T) {
	id := [16]byte{0x01, 0x92, 0xf0, 0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 0x01}
	if got, err := ParseKeyID(keyring.FormatID(id)); err != nil || got != id {
		t.Fatalf("ParseKeyID(FormatID) = %x, %v", got, err)
	}
	if got, err := ParseKEKID(FormatKEKID(id)); err != nil || got != id {
		t.Fatalf("ParseKEKID(FormatKEKID) = %x, %v", got, err)
	}
	for _, bad := range []string{"", "nope", "0192f000000070008000000000000001", "0192f000-0000-7000-8000-00000000000", "0192f0-00000-7000-8000-000000000001"} {
		if _, err := ParseKeyID(bad); !errors.Is(err, ErrBadKeyID) {
			t.Errorf("ParseKeyID(%q) = %v, want ErrBadKeyID", bad, err)
		}
	}
	for _, bad := range []string{"", "zz", keyring.FormatID(id), "00112233"} {
		if _, err := ParseKEKID(bad); !errors.Is(err, ErrBadKeyID) {
			t.Errorf("ParseKEKID(%q) = %v, want ErrBadKeyID", bad, err)
		}
	}
}

// Rewrap refuses a KEK this process does not hold before it reads anything, so a typo'd id never moves the target.
func TestRewrapRefusesBeforeTheDatabase(t *testing.T) {
	ctx := context.Background()
	held, err := local.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(nil, [][]byte{testKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rewrap(ctx, FormatKEKID([16]byte{9})); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("rewrap to a KEK not held: %v", err)
	}
	if _, err := b.Rewrap(ctx, "not-hex"); !errors.Is(err, ErrBadKeyID) {
		t.Fatalf("rewrap to a malformed id: %v", err)
	}
	none, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := none.Rewrap(ctx, FormatKEKID(held.ID())); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("rewrap without a master key: %v", err)
	}
}

func TestDestroyErrorExplains(t *testing.T) {
	for err, want := range map[error]string{
		keyring.ErrIsPrimary: "rotate its scope first",
		keyring.ErrInUse:     "row count reaches 0",
		keyring.ErrTooRecent: "24h0m0s after it was demoted",
	} {
		got := destroyError("k1", err)
		if !errors.Is(got, err) || !strings.Contains(got.Error(), want) || !strings.Contains(got.Error(), "k1") {
			t.Errorf("destroyError(%v) = %v", err, got)
		}
	}
}
