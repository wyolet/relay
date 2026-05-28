package bitwarden

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

func TestResolver_EndToEnd_PersonalItem(t *testing.T) {
	email := "user@example.com"
	password := "master-password"
	iterations := 600000

	masterKey, err := makeMasterKey(password, email, kdfPBKDF2, iterations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	userSymKey := symmetricKey{
		encKey: make([]byte, 32),
		macKey: make([]byte, 32),
	}
	for i := range userSymKey.encKey {
		userSymKey.encKey[i] = byte(i + 1)
	}
	for i := range userSymKey.macKey {
		userSymKey.macKey[i] = byte(i + 33)
	}

	userSymKeyBytes := append(append([]byte{}, userSymKey.encKey...), userSymKey.macKey...)
	stretched, err := stretchKey(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	encryptedUserKey := encryptType2(t, stretched, userSymKeyBytes)

	loginName := encryptType2String(t, userSymKey, "openai-key")
	loginPassword := encryptType2String(t, userSymKey, "sk-secret-value")
	loginUsername := encryptType2String(t, userSymKey, "api-user")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/identity/accounts/prelogin":
			json.NewEncoder(w).Encode(preloginResponse{
				KDF:           kdfPBKDF2,
				KDFIterations: iterations,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/identity/connect/token":
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken:  "access-token",
				ExpiresIn:    3600,
				RefreshToken: "refresh-token",
				Key:          encryptedUserKey,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/sync":
			json.NewEncoder(w).Encode(syncResponse{
				Profile: syncProfile{Email: email},
				Ciphers: []syncCipher{{
					ID:   "cipher-personal",
					Type: cipherTypeLogin,
					Name: loginName,
					Login: &syncLogin{
						Username: strPtr(loginUsername),
						Password: strPtr(loginPassword),
					},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	resolver := New(Config{
		BaseURL:        srv.URL,
		Email:          email,
		MasterPassword: password,
		ClientID:       "client-id",
		ClientSecret:   "client-secret",
		SyncInterval:   time.Hour,
		DeviceID:       "test-device",
	})

	ctx := context.Background()
	waitForSync(t, resolver)

	got, err := resolver.Resolve(ctx, secret.Ref{Kind: secret.KindBitwarden, Path: "openai-key/password"})
	if err != nil {
		t.Fatalf("Resolve password: %v", err)
	}
	if string(got) != "sk-secret-value" {
		t.Errorf("password = %q, want sk-secret-value", got)
	}

	got, err = resolver.Resolve(ctx, secret.Ref{Kind: secret.KindBitwarden, Path: "openai-key/username"})
	if err != nil {
		t.Fatalf("Resolve username: %v", err)
	}
	if string(got) != "api-user" {
		t.Errorf("username = %q, want api-user", got)
	}

	got, err = resolver.Resolve(ctx, secret.Ref{Kind: secret.KindBitwarden, Path: "cipher-personal/password"})
	if err != nil {
		t.Fatalf("Resolve by id: %v", err)
	}
	if string(got) != "sk-secret-value" {
		t.Errorf("password by id = %q, want sk-secret-value", got)
	}
}

func TestResolver_EndToEnd_OrgItem(t *testing.T) {
	email := "user@example.com"
	password := "master-password"
	iterations := 600000

	masterKey, err := makeMasterKey(password, email, kdfPBKDF2, iterations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	userSymKey := symmetricKey{
		encKey: make([]byte, 32),
		macKey: make([]byte, 32),
	}
	for i := range userSymKey.encKey {
		userSymKey.encKey[i] = byte(i + 10)
	}
	for i := range userSymKey.macKey {
		userSymKey.macKey[i] = byte(i + 42)
	}

	orgSymKey := symmetricKey{
		encKey: make([]byte, 32),
		macKey: make([]byte, 32),
	}
	for i := range orgSymKey.encKey {
		orgSymKey.encKey[i] = byte(i + 100)
	}
	for i := range orgSymKey.macKey {
		orgSymKey.macKey[i] = byte(i + 132)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	stretched, err := stretchKey(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	userSymKeyBytes := append(append([]byte{}, userSymKey.encKey...), userSymKey.macKey...)
	encryptedUserKey := encryptType2(t, stretched, userSymKeyBytes)

	privDER, err := marshalPKCS8(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	encryptedPrivateKey := encryptType2(t, userSymKey, privDER)

	orgKeyPlain := append(append([]byte{}, orgSymKey.encKey...), orgSymKey.macKey...)
	orgKeyCT, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &rsaKey.PublicKey, orgKeyPlain, nil)
	if err != nil {
		t.Fatal(err)
	}
	encryptedOrgKey := "4." + base64Encode(orgKeyCT)

	orgID := "org-abc"
	orgItemName := encryptType2String(t, orgSymKey, "org-secret")
	orgItemPassword := encryptType2String(t, orgSymKey, "org-password-value")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/identity/accounts/prelogin":
			json.NewEncoder(w).Encode(preloginResponse{KDF: kdfPBKDF2, KDFIterations: iterations})
		case r.Method == http.MethodPost && r.URL.Path == "/identity/connect/token":
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken:  "access-token",
				ExpiresIn:    3600,
				RefreshToken: "refresh-token",
				Key:          encryptedUserKey,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/sync":
			json.NewEncoder(w).Encode(syncResponse{
				Profile: syncProfile{
					Email:      email,
					PrivateKey: encryptedPrivateKey,
					Organizations: []syncOrganization{{
						ID:  orgID,
						Key: encryptedOrgKey,
					}},
				},
				Ciphers: []syncCipher{{
					ID:             "cipher-org",
					Type:           cipherTypeLogin,
					OrganizationID: &orgID,
					Name:           orgItemName,
					Login:          &syncLogin{Password: strPtr(orgItemPassword)},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	resolver := New(Config{
		BaseURL:        srv.URL,
		Email:          email,
		MasterPassword: password,
		ClientID:       "client-id",
		ClientSecret:   "client-secret",
		SyncInterval:   time.Hour,
		DeviceID:       "test-device",
	})

	waitForSync(t, resolver)

	got, err := resolver.Resolve(context.Background(), secret.Ref{Kind: secret.KindBitwarden, Path: "org-secret/password"})
	if err != nil {
		t.Fatalf("Resolve org item: %v", err)
	}
	if string(got) != "org-password-value" {
		t.Errorf("password = %q, want org-password-value", got)
	}
}

func TestResolver_AmbiguousName(t *testing.T) {
	email := "user@example.com"
	password := "master-password"
	iterations := 600000

	masterKey, _ := makeMasterKey(password, email, kdfPBKDF2, iterations, nil, nil)
	userSymKey := symmetricKey{encKey: bytes32(1), macKey: bytes32(2)}
	stretched, _ := stretchKey(masterKey)
	userSymKeyBytes := append(userSymKey.encKey, userSymKey.macKey...)
	encryptedUserKey := encryptType2(t, stretched, userSymKeyBytes)

	dupName := encryptType2String(t, userSymKey, "duplicate")
	pass1 := encryptType2String(t, userSymKey, "pass1")
	pass2 := encryptType2String(t, userSymKey, "pass2")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity/accounts/prelogin":
			json.NewEncoder(w).Encode(preloginResponse{KDF: kdfPBKDF2, KDFIterations: iterations})
		case "/identity/connect/token":
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "t", ExpiresIn: 3600, RefreshToken: "r", Key: encryptedUserKey})
		case "/api/sync":
			json.NewEncoder(w).Encode(syncResponse{
				Ciphers: []syncCipher{
					{ID: "1", Type: cipherTypeLogin, Name: dupName, Login: &syncLogin{Password: strPtr(pass1)}},
					{ID: "2", Type: cipherTypeLogin, Name: dupName, Login: &syncLogin{Password: strPtr(pass2)}},
				},
			})
		}
	}))
	defer srv.Close()

	resolver := New(Config{
		BaseURL: srv.URL, Email: email, MasterPassword: password,
		ClientID: "id", ClientSecret: "secret", SyncInterval: time.Hour, DeviceID: "dev",
	})
	waitForSync(t, resolver)

	_, err := resolver.Resolve(context.Background(), secret.Ref{Kind: secret.KindBitwarden, Path: "duplicate/password"})
	if err == nil {
		t.Fatal("expected ambiguous name error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("unexpected error: %v", err)
	}
}

func waitForSync(t *testing.T, r *Resolver) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.RLock()
		done := r.syncDone
		err := r.syncErr
		r.mu.RUnlock()
		if done {
			if err != nil {
				t.Fatalf("sync failed: %v", err)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("sync timed out")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func strPtr(s string) *string { return &s }

func bytes32(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func marshalPKCS8(key *rsa.PrivateKey) ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(key)
}

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
