// Package ids generates immutable resource identifiers.
//
// Catalog rows use UUIDv7 as their primary key — time-sortable, std-blessed,
// 36-char canonical form. Generation is app-side; PG never invents an id.
package ids

import "github.com/google/uuid"

// New returns a new UUIDv7 string. Panics only if the system RNG fails.
func New() string {
	return uuid.Must(uuid.NewV7()).String()
}

// Valid reports whether s parses as a UUID (any version). Used at the
// admin boundary to reject malformed id path params before storage.
func Valid(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// UnixSec extracts the embedded creation time (seconds since epoch) from a
// UUIDv7 string. Returns 0 if s is not a parseable UUIDv7. UUIDv7 encodes
// milliseconds in the first 48 bits, so seconds-precision is exact.
//
// Used by surfaces that expose OpenAI-shape `created` fields (e.g.
// /v1/models) without adding a separate CreatedAt column to every entity.
func UnixSec(s string) int64 {
	u, err := uuid.Parse(s)
	if err != nil || u.Version() != 7 {
		return 0
	}
	sec, _ := u.Time().UnixTime()
	return sec
}
