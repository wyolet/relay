package bitwarden

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

const (
	kdfPBKDF2   = 0
	kdfArgon2id = 1
)

func makeMasterKey(password, email string, kdfType, iterations int, memory, parallelism *int) ([]byte, error) {
	salt := []byte(strings.ToLower(strings.TrimSpace(email)))

	switch kdfType {
	case kdfPBKDF2:
		if iterations < 1 {
			return nil, fmt.Errorf("bitwarden: PBKDF2 iterations must be >= 1, got %d", iterations)
		}
		return pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New), nil

	case kdfArgon2id:
		mem := 64 * 1024
		par := 4
		if memory != nil {
			mem = *memory * 1024
		}
		if parallelism != nil {
			par = *parallelism
		}
		return argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(mem), uint8(par), 32), nil

	default:
		return nil, fmt.Errorf("bitwarden: unsupported KDF type: %d", kdfType)
	}
}

func stretchKey(masterKey []byte) (symmetricKey, error) {
	encKey := make([]byte, 32)
	r := hkdf.Expand(sha256.New, masterKey, []byte("enc"))
	if _, err := io.ReadFull(r, encKey); err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: hkdf expand enc: %w", err)
	}

	macKey := make([]byte, 32)
	r = hkdf.Expand(sha256.New, masterKey, []byte("mac"))
	if _, err := io.ReadFull(r, macKey); err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: hkdf expand mac: %w", err)
	}

	return symmetricKey{encKey: encKey, macKey: macKey}, nil
}

func decryptSymmetricKey(encryptedKey string, masterKey []byte) (symmetricKey, error) {
	cs, err := parseCipherString(encryptedKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: parse encrypted key: %w", err)
	}

	stretched, err := stretchKey(masterKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: stretch key: %w", err)
	}

	decrypted, err := cs.decrypt(stretched)
	if err != nil {
		legacy := symmetricKey{encKey: masterKey}
		decrypted, err = cs.decrypt(legacy)
		if err != nil {
			return symmetricKey{}, fmt.Errorf("bitwarden: decrypt symmetric key: %w", err)
		}
	}

	if len(decrypted) != 64 {
		return symmetricKey{}, fmt.Errorf("bitwarden: unexpected symmetric key length: %d (expected 64)", len(decrypted))
	}

	return symmetricKey{
		encKey: decrypted[:32],
		macKey: decrypted[32:],
	}, nil
}

func decryptPrivateKey(encryptedPrivateKey string, symKey symmetricKey) (*rsa.PrivateKey, error) {
	if encryptedPrivateKey == "" {
		return nil, errors.New("bitwarden: encrypted private key is empty")
	}

	cs, err := parseCipherString(encryptedPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: parse private key cipher string: %w", err)
	}

	derBytes, err := cs.decrypt(symKey)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: decrypt private key: %w", err)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(derBytes)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: parse PKCS8 private key: %w", err)
	}

	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("bitwarden: private key is not RSA")
	}

	return rsaKey, nil
}

func decryptOrgKey(encryptedOrgKey string, privateKey *rsa.PrivateKey) (symmetricKey, error) {
	if encryptedOrgKey == "" {
		return symmetricKey{}, errors.New("bitwarden: encrypted org key is empty")
	}

	cs, err := parseCipherString(encryptedOrgKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: parse org key cipher string: %w", err)
	}

	decrypted, err := cs.decryptRSA(privateKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: RSA decrypt org key: %w", err)
	}

	if len(decrypted) != 64 {
		return symmetricKey{}, fmt.Errorf("bitwarden: unexpected org key length: %d (expected 64)", len(decrypted))
	}

	return symmetricKey{
		encKey: decrypted[:32],
		macKey: decrypted[32:],
	}, nil
}
