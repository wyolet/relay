package payload

import (
	"context"
	"errors"
)

// ErrEraseUnsupported is returned by an Eraser that fronts a backend unable
// to delete captured bodies.
var ErrEraseUnsupported = errors.New("payload: backend cannot erase captured bodies")

// Eraser deletes captured bodies. Implemented by backends that can.
type Eraser interface {
	Erase(ctx context.Context, f EraseFilter) (EraseResult, error)
}

// EraseFilter selects the records to erase by owner. At least one field is
// set; both narrow to one principal's records within one project.
type EraseFilter struct {
	ProjectID   string
	PrincipalID string
}

// EraseResult reports what an erase matched.
type EraseResult struct {
	// Requests is the number of request records matched before deletion.
	Requests uint64
}
