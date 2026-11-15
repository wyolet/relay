package bitwarden

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

func TestMakeMasterKey(t *testing.T) {
	mem, par := 8, 2
	// The salt is the trimmed, lower-cased email.
	const email, salt = "  User@Example.COM ", "user@example.com"
	for _, tc := range []struct {
		name      string
		kdf, iter int
		mem, par  *int
		want      []byte
		wantErr   string
	}{
		{"pbkdf2", kdfPBKDF2, 1000, nil, nil, pbkdf2.Key([]byte("password123"), []byte(salt), 1000, 32, sha256.New), ""},
		{"pbkdf2 ignores argon2 settings", kdfPBKDF2, 2, &mem, &par, pbkdf2.Key([]byte("password123"), []byte(salt), 2, 32, sha256.New), ""},
		{"pbkdf2 without iterations", kdfPBKDF2, 0, nil, nil, nil, "bitwarden: PBKDF2 iterations must be >= 1, got 0"},
		{"unknown kdf", 2, 1000, nil, nil, nil, "bitwarden: unsupported KDF type: 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := makeMasterKey("password123", email, tc.kdf, tc.iter, tc.mem, tc.par)
			if !bytes.Equal(got, tc.want) || !wantErr(err, tc.wantErr) {
				t.Errorf("makeMasterKey = %x, %v; want %x, %q", got, err, tc.want, tc.wantErr)
			}
		})
	}

	// Holds whatever the salt is: every setting reaches the derivation, and the defaults are 64 MiB and 4 lanes.
	t.Run("argon2id settings", func(t *testing.T) {
		big, four, one := 64, 4, 1
		derive := func(password string, iter int, mem, par *int) string {
			key, err := makeMasterKey(password, email, kdfArgon2id, iter, mem, par)
			if err != nil || len(key) != 32 {
				t.Fatalf("makeMasterKey = %x, %v", key, err)
			}
			return string(key)
		}
		base := derive("password123", 1, &mem, &par)
		if derive("password123", 1, &mem, &par) != base {
			t.Error("the same settings derived two keys")
		}
		for name, other := range map[string]string{
			"password":    derive("password124", 1, &mem, &par),
			"iterations":  derive("password123", 2, &mem, &par),
			"memory":      derive("password123", 1, &par, &par),
			"parallelism": derive("password123", 1, &mem, &one),
		} {
			if other == base {
				t.Errorf("changing the %s left the key unchanged", name)
			}
		}
		if derive("password123", 1, nil, nil) != derive("password123", 1, &big, &four) {
			t.Error("defaults are not 64 MiB and 4 lanes")
		}
	})

	t.Run("argon2id", func(t *testing.T) {
		hashed := sha256.Sum256([]byte(salt))
		for _, tc := range []struct {
			name     string
			mem, par *int
			want     []byte
		}{
			{"account settings", &mem, &par, argon2.IDKey([]byte("password123"), hashed[:], 3, 8*1024, 2, 32)},
			{"defaults: 64 MiB, 4 lanes", nil, nil, argon2.IDKey([]byte("password123"), hashed[:], 3, 64*1024, 4, 32)},
		} {
			got, err := makeMasterKey("password123", email, kdfArgon2id, 3, tc.mem, tc.par)
			if !bytes.Equal(got, tc.want) || err != nil {
				t.Errorf("%s: makeMasterKey = %x, %v; want %x", tc.name, got, err, tc.want)
			}
		}
	})

	t.Run("argon2id settings the library refuses", func(t *testing.T) {
		zero, lanes256, negative, huge := 0, 256, -1, 1<<20
		for name, call := range map[string]func() ([]byte, error){
			"no iterations":   func() ([]byte, error) { return makeMasterKey("p", email, kdfArgon2id, 0, &mem, &par) },
			"no lanes":        func() ([]byte, error) { return makeMasterKey("p", email, kdfArgon2id, 3, &mem, &zero) },
			"256 lanes":       func() ([]byte, error) { return makeMasterKey("p", email, kdfArgon2id, 3, &mem, &lanes256) },
			"negative memory": func() ([]byte, error) { return makeMasterKey("p", email, kdfArgon2id, 3, &negative, &par) },
			"1 TiB of memory": func() ([]byte, error) { return makeMasterKey("p", email, kdfArgon2id, 3, &huge, &par) },
		} {
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("%s: panic: %v", name, p)
					}
				}()
				if got, err := call(); err == nil {
					t.Errorf("%s: makeMasterKey = %x, want an error", name, got)
				}
			}()
		}
	})
}

func TestStretchKey(t *testing.T) {
	master := seq(0, 32)
	expand := func(info string) []byte {
		out := make([]byte, 32)
		if _, err := hkdf.Expand(sha256.New, master, []byte(info)).Read(out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got, err := stretchKey(master)
	want := symmetricKey{encKey: expand("enc"), macKey: expand("mac")}
	if d := cmp.Diff(want, got, cmp.AllowUnexported(symmetricKey{})); d != "" || err != nil {
		t.Errorf("stretchKey: %v (-want +got):\n%s", err, d)
	}
}

func TestDecryptSymmetricKey(t *testing.T) {
	master := seq(7, 32)
	stretched, err := stretchKey(master)
	must(t, err)
	user := symKey(1)
	userBytes := append(bytes.Clone(user.encKey), user.macKey...)
	const failed = "bitwarden: decrypt symmetric key: bitwarden: MAC key required for type 2 decryption"

	for _, tc := range []struct {
		name, in string
		master   []byte
		want     symmetricKey
		wantErr  string
	}{
		{"under the stretched master key", encryptType2(t, stretched, userBytes), master, user, ""},
		{"legacy: type 0 under the master key itself", encryptType0(t, master, userBytes), master, user, ""},
		{"another master password", encryptType2(t, stretched, userBytes), seq(8, 32), symmetricKey{}, failed},
		{"mac made with another key", encryptType2(t, symmetricKey{encKey: stretched.encKey, macKey: seq(9, 32)}, userBytes), master, symmetricKey{}, failed},
		{"32-byte key", encryptType2(t, stretched, user.encKey), master, symmetricKey{}, "bitwarden: unexpected symmetric key length: 32 (expected 64)"},
		{"empty", "", master, symmetricKey{}, "bitwarden: parse encrypted key: bitwarden: empty cipher string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decryptSymmetricKey(tc.in, tc.master)
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(symmetricKey{})); d != "" || !wantErr(err, tc.wantErr) {
				t.Errorf("decryptSymmetricKey: %v, want %q (-want +got):\n%s", err, tc.wantErr, d)
			}
		})
	}
}

func TestDecryptPrivateKey(t *testing.T) {
	key := symKey(0)
	der, err := x509.MarshalPKCS8PrivateKey(testRSAKey())
	must(t, err)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	must(t, err)

	got, err := decryptPrivateKey(encryptType2(t, key, der), key)
	if err != nil || !got.Equal(testRSAKey()) {
		t.Fatalf("decryptPrivateKey: %v; key differs from the one sealed", err)
	}
	for _, tc := range []struct{ name, in, wantErr string }{
		{"empty", "", "bitwarden: encrypted private key is empty"},
		{"not a cipher string", "plain", "bitwarden: parse private key cipher string: bitwarden: invalid cipher string: missing type separator"},
		{"another user's key", encryptType2(t, symKey(1), der), "bitwarden: decrypt private key: bitwarden: MAC verification failed"},

		{"not rsa", encryptType2(t, key, ecDER), "bitwarden: private key is not RSA"},
	} {
		if got, err := decryptPrivateKey(tc.in, key); got != nil || !wantErr(err, tc.wantErr) {
			t.Errorf("%s: decryptPrivateKey = %v, %v; want %q", tc.name, got, err, tc.wantErr)
		}
	}
	if got, err := decryptPrivateKey(encryptType2String(t, key, "junk"), key); got != nil || err == nil || !strings.HasPrefix(err.Error(), "bitwarden: parse PKCS8 private key: ") {
		t.Errorf("not a key: decryptPrivateKey = %v, %v", got, err)
	}
}

func TestDecryptOrgKey(t *testing.T) {
	org := symKey(100)
	orgBytes := append(bytes.Clone(org.encKey), org.macKey...)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	for _, tc := range []struct {
		name, in string
		priv     *rsa.PrivateKey
		want     symmetricKey
		wantErr  string
	}{
		{"oaep sha-1", rsaEncrypt(t, "4", sha1.New(), orgBytes), testRSAKey(), org, ""},
		{"oaep sha-256", rsaEncrypt(t, "3", sha256.New(), orgBytes), testRSAKey(), org, ""},
		{"sha-1 ciphertext labelled sha-256", rsaEncrypt(t, "3", sha1.New(), orgBytes), testRSAKey(), symmetricKey{}, "bitwarden: RSA decrypt org key: crypto/rsa: decryption error"},
		{"another member's private key", rsaEncrypt(t, "4", sha1.New(), orgBytes), other, symmetricKey{}, "bitwarden: RSA decrypt org key: crypto/rsa: decryption error"},
		{"32-byte key", rsaEncrypt(t, "4", sha1.New(), org.encKey), testRSAKey(), symmetricKey{}, "bitwarden: unexpected org key length: 32 (expected 64)"},
		{"symmetric cipher string", encryptType2(t, org, orgBytes), testRSAKey(), symmetricKey{}, "bitwarden: RSA decrypt org key: bitwarden: not an RSA cipher type: 2"},
		{"not a cipher string", "plain", testRSAKey(), symmetricKey{}, "bitwarden: parse org key cipher string: bitwarden: invalid cipher string: missing type separator"},
		{"empty", "", testRSAKey(), symmetricKey{}, "bitwarden: encrypted org key is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decryptOrgKey(tc.in, tc.priv)
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(symmetricKey{})); d != "" || !wantErr(err, tc.wantErr) {
				t.Errorf("decryptOrgKey: %v, want %q (-want +got):\n%s", err, tc.wantErr, d)
			}
		})
	}
}
