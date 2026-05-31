// Package payload defines jobq's payload storage abstraction and its default
// file backend. jobq keeps payload bytes out of the Postgres row — the row
// holds only a URI returned by a Store. The file backend is the zero-dependency
// default; an object-store backend (s3) is a future sibling implementing the
// same interface.
package payload

import "context"

// Store persists opaque payload blobs addressed by an opaque URI. A URI is
// produced by Put and is only meaningful to the Store that produced it.
//
// Implementations must be safe for concurrent use.
type Store interface {
	// Put stores data under a caller-chosen key and returns a URI that Get can
	// later resolve. The key is a hint for organisation (jobq passes
	// "<jobID>/input" and "<jobID>/result"); the URI is authoritative.
	Put(ctx context.Context, key string, data []byte) (uri string, err error)

	// Get returns the bytes previously stored at uri.
	Get(ctx context.Context, uri string) ([]byte, error)

	// Delete removes the blob at uri. Deleting a missing blob is not an error.
	Delete(ctx context.Context, uri string) error
}
