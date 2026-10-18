package aead

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
)

// xaes is XAES-256-GCM as specified at https://c2sp.org/XAES-256-GCM: AES-256-GCM under a key derived per 192-bit nonce, so random nonces stay safe for any realistic number of messages under one key.
type xaes struct {
	block cipher.Block
	k1    [aes.BlockSize]byte
}

func newXAES(key []byte) (*xaes, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	x := &xaes{block: block}
	// K1 is the CMAC subkey of K: L = AES-256_K(0^128), shifted left one bit, conditionally xored with 0x87.
	block.Encrypt(x.k1[:], x.k1[:])
	msb := x.k1[0] >> 7
	for i := 0; i < len(x.k1)-1; i++ {
		x.k1[i] = x.k1[i]<<1 | x.k1[i+1]>>7
	}
	x.k1[len(x.k1)-1] = x.k1[len(x.k1)-1]<<1 ^ msb*0x87
	return x, nil
}

// derive returns the AES-256-GCM key and 96-bit nonce for a 192-bit XAES nonce.
func (x *xaes) derive(nonce []byte) (key, gcmNonce []byte) {
	var m1, m2 [aes.BlockSize]byte
	m1[1], m1[2] = 0x01, 'X'
	copy(m1[4:], nonce[:12])
	m2 = m1
	m2[1] = 0x02
	subtle.XORBytes(m1[:], m1[:], x.k1[:])
	subtle.XORBytes(m2[:], m2[:], x.k1[:])
	key = make([]byte, 32)
	x.block.Encrypt(key[:16], m1[:])
	x.block.Encrypt(key[16:], m2[:])
	return key, nonce[12:]
}

func (x *xaes) gcm(nonce []byte) (cipher.AEAD, []byte, error) {
	key, gcmNonce := x.derive(nonce)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	return g, gcmNonce, nil
}

func (x *xaes) seal(dst, nonce, plaintext, aad []byte) ([]byte, error) {
	g, n, err := x.gcm(nonce)
	if err != nil {
		return nil, err
	}
	return g.Seal(dst, n, plaintext, aad), nil
}

func (x *xaes) open(dst, nonce, ciphertext, aad []byte) ([]byte, error) {
	g, n, err := x.gcm(nonce)
	if err != nil {
		return nil, err
	}
	return g.Open(dst, n, ciphertext, aad)
}
