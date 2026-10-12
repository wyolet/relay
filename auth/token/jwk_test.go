package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// rfc8037Key is the Ed25519 key of RFC 8037 Appendix A.1.
func rfc8037Key(t *testing.T) ed25519.PublicKey {
	t.Helper()
	seed, err := base64.RawURLEncoding.DecodeString("nWGxne_9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return pub
}

func TestJWK_RFC8037Vector(t *testing.T) {
	pub := rfc8037Key(t)
	k, err := JWK(pub)
	if err != nil {
		t.Fatalf("JWK: %v", err)
	}
	if k.X != "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo" {
		t.Errorf("x = %q, want the RFC 8037 A.2 value", k.X)
	}
	if k.Kid != KeyID(pub) {
		t.Errorf("kid = %q, want KeyID %q so it matches the token header", k.Kid, KeyID(pub))
	}
	raw, err := json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo","kid":"` + KeyID(pub) + `","use":"sig","alg":"EdDSA"}`
	if string(raw) != want {
		t.Errorf("JSON =\n %s\nwant\n %s", raw, want)
	}
}

func TestJWK_RejectsWrongLength(t *testing.T) {
	if _, err := JWK(ed25519.PublicKey{1, 2, 3}); err == nil {
		t.Error("JWK accepted a 3-byte key")
	}
	if _, err := JWKSet(rfc8037Key(t), nil); err == nil {
		t.Error("JWKSet accepted a nil key")
	}
}

func TestJWKSet_OrderAndDuplicates(t *testing.T) {
	a := rfc8037Key(t)
	b, _ := testKey(t)
	set, err := JWKSet(b, a, b)
	if err != nil {
		t.Fatalf("JWKSet: %v", err)
	}
	if len(set.Keys) != 2 || set.Keys[0].Kid != KeyID(b) || set.Keys[1].Kid != KeyID(a) {
		t.Fatalf("keys = %+v, want b then a, once each", set.Keys)
	}

	empty, err := JWKSet()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"keys":[]}` {
		t.Errorf("empty set = %s, want {\"keys\":[]}", raw)
	}
}
