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
