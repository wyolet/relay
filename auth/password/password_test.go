package password

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestVerify(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		stored, given   string
		wantOK, wantPln bool
	}{
		{"bcrypt match", string(hash), "s3cret", true, false},
		{"bcrypt mismatch", string(hash), "wrong", false, false},
		{"plain match", "legacy-plain", "legacy-plain", true, true},
		{"plain mismatch", "legacy-plain", "other", false, true},
		{"empty stored never matches", "", "anything", false, false},
		{"empty password never matches", string(hash), "", false, false},
		{"empty password against plain", "legacy-plain", "", false, false},
		{"both empty", "", "", false, false},
		{"the hash itself is not the password", string(hash), string(hash), false, false},
		{"plain prefix of the stored value", "legacy-plain", "legacy", false, true},
		{"plain differs in case", "legacy-plain", "Legacy-plain", false, true},
		{"truncated hash", string(hash[:len(hash)-1]), "s3cret", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, plain := Verify(tc.stored, tc.given)
			if ok != tc.wantOK || plain != tc.wantPln {
				t.Errorf("Verify = (%v, %v), want (%v, %v)", ok, plain, tc.wantOK, tc.wantPln)
			}
		})
	}
}

func TestHash(t *testing.T) {
	hash, err := Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != bcrypt.DefaultCost || !IsBcryptHash(hash) {
		t.Fatalf("Hash = %q (cost %d, %v), want a bcrypt hash at the default cost", hash, cost, err)
	}
	if ok, plain := Verify(hash, "s3cret"); !ok || plain {
		t.Errorf("Verify of Hash output = (%v, %v)", ok, plain)
	}
	if again, err := Hash("s3cret"); err != nil || again == hash {
		t.Errorf("a second Hash of the same password = %q, %v; want a fresh salt", again, err)
	}
	// bcrypt reads at most 72 bytes and refuses more rather than truncating.
	if h, err := Hash(string(make([]byte, 73))); err != bcrypt.ErrPasswordTooLong || h != "" {
		t.Errorf("Hash of a 73-byte password = %q, %v", h, err)
	}
}

func TestIsBcryptHash(t *testing.T) {
	for s, want := range map[string]bool{
		"$2a$10$abc": true,
		"$2b$10$abc": true,
		"$2y$10$abc": true,
		"$2x$10$abc": false,
		"plain":      false,
		"":           false,
	} {
		if got := IsBcryptHash(s); got != want {
			t.Errorf("IsBcryptHash(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestIsPlaceholder(t *testing.T) {
	for _, pw := range []string{"change-me-please", "change-me", "CHANGEME", "Password"} {
		if !IsPlaceholder(pw) {
			t.Errorf("IsPlaceholder(%q) = false, want true", pw)
		}
	}
	if IsPlaceholder("c0rrect-horse-battery") {
		t.Error("a real password reported as a placeholder")
	}
}
