package main

import (
	"testing"

	"github.com/wyolet/relay/pkg/crypto"
)

func TestParseMasterKeyBootBehavior(t *testing.T) {
	// Valid key: ParseMasterKey succeeds.
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := crypto.ParseMasterKey(key)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if len(b) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(b))
	}

	// Garbage key: ParseMasterKey returns error (simulates RELAY_MASTER_KEY=garbage → exit 1).
	_, err = crypto.ParseMasterKey("garbage")
	if err == nil {
		t.Fatal("expected error for garbage key")
	}

	// Unset (empty string): treated as unset — no error expected by the boot logic.
	// Boot code only calls ParseMasterKey when raw != "".
	// Nothing to assert here beyond the above.
}

func TestMasterKeyGenerateRoundTrip(t *testing.T) {
	s, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.ParseMasterKey(s); err != nil {
		t.Fatalf("GenerateMasterKey → ParseMasterKey failed: %v", err)
	}
}
