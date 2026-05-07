package catalog

import "errors"

// Domain sentinel errors returned by the catalog layer.
// Storage translates pgx-level errors to these; callers use errors.Is.
var (
	ErrNotFound    = errors.New("catalog: not found")
	ErrConflict    = errors.New("catalog: name conflict")
	ErrInvalidSpec = errors.New("catalog: invalid spec")
)
