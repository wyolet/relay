package directory

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
)

// Owner kinds the module gives a meaning to. Any other kind is the product's and passes through untouched.
const (
	OwnerSystem  = "system"
	OwnerUser    = "user"
	OwnerTeam    = "team"
	OwnerProject = "project"
)

// Global is the scope every chain ends in and the scope a binding names to apply everywhere.
var Global = rbac.Scope{Kind: OwnerSystem}

var (
	// ErrNotFound reports an operation on a row that does not exist.
	ErrNotFound = errors.New("directory: not found")
	// ErrInvalid reports a row that fails validation or references a row that does not exist.
	ErrInvalid = errors.New("directory: invalid")
	// ErrConflict reports a write that would duplicate a unique name, username, email or identity.
	ErrConflict = errors.New("directory: conflict")
	// ErrEmailTaken is the ErrConflict of an account whose email another account holds.
	ErrEmailTaken = fmt.Errorf("%w: email is taken", ErrConflict)
	// ErrStaleResourceVersion reports a conditional update whose ResourceVersion no longer matches the stored row.
	ErrStaleResourceVersion = errors.New("directory: resource changed since it was read")
)

// Metadata is the identity every row except a user carries.
type Metadata struct {
	// ID is the immutable primary key; a create mints a UUIDv7 when it is empty.
	ID string
	// Name is a DNS-1123 label, unique per kind; a create derives it from DisplayName when it is empty.
	Name        string
	DisplayName string
	Description string
	// Owner is who created or manages the row. Projects, service accounts and role bindings derive it from their spec.
	Owner       authz.Owner
	Labels      map[string]string
	Annotations map[string]string
	// Dirty marks a row an operator edited after a loader wrote it, so the loader can leave it alone.
	Dirty bool
	// ResourceVersion changes on every write. An update carrying it applies only while the stored row still has it; empty updates unconditionally.
	ResourceVersion string
	// CreatedAt and UpdatedAt are set by the store.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ExpectedVersion returns the version a conditional update must match; ok is false when ResourceVersion is empty. A value no stored row can carry is ErrStaleResourceVersion.
func (m Metadata) ExpectedVersion() (v int64, ok bool, err error) {
	if m.ResourceVersion == "" {
		return 0, false, nil
	}
	v, err = strconv.ParseInt(m.ResourceVersion, 10, 64)
	if err != nil {
		return 0, false, ErrStaleResourceVersion
	}
	return v, true, nil
}

// FormatResourceVersion renders a stored version counter as a ResourceVersion.
func FormatResourceVersion(v int64) string { return strconv.FormatInt(v, 10) }

// NewID returns a fresh UUIDv7. Panics only if the system random source fails.
func NewID() string { return uuid.Must(uuid.NewV7()).String() }

func enabled(b *bool) bool { return b == nil || *b }
