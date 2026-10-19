package keyring

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// newKeyID returns a UUIDv7 (RFC 9562): 48-bit Unix milliseconds, version and variant bits, random rest.
func newKeyID(now time.Time) ([16]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("keyring: read key id: %w", err)
	}
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli()))
	copy(id[:6], ms[2:])
	id[6] = id[6]&0x0f | 0x70
	id[8] = id[8]&0x3f | 0x80
	return id, nil
}

// FormatID renders a key id in the canonical UUID text form.
func FormatID(id [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], id[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], id[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], id[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], id[8:10])
	b[23] = '-'
	hex.Encode(b[24:], id[10:])
	return string(b[:])
}

// wrapAAD binds a wrapped DEK to its id and scope; the id is fixed-length, so the concatenation is unambiguous.
func wrapAAD(id [16]byte, scope string) []byte {
	b := make([]byte, 0, len(id)+len(scope))
	b = append(b, id[:]...)
	return append(b, scope...)
}
