// Package meta holds the identity primitives every domain entity carries:
// UUIDv7 id, DNS-1123 slug name, free-text display name, and provenance.
//
// No I/O, no business logic — just value types and tiny pure helpers.
// Everything else (entity domain packages, storage, handlers) depends on this;
// it depends on nothing in the project.
package meta

import (
	"regexp"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/wyolet/relay/pkg/ids"
)

// Validator is the shared go-playground/validator instance every entity
// package uses. Custom tags ("slug", "uuid7", ...) are registered here.
// Entities call Validator.Struct(...) from their Validate() methods.
var Validator = func() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("slug", isDNS1123Slug); err != nil {
		panic("meta: register slug validator: " + err.Error())
	}
	return v
}()

// dnsLabel mirrors the K8s DNS-1123 label rule used for metadata.Name. Lowercase
// letters, digits, hyphens; must not start/end with a hyphen; ≤63 chars.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func isDNS1123Slug(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	return dnsLabel.MatchString(s)
}

// Metadata is the identity tuple stamped on every domain row.
//
//   - ID is the immutable primary key (UUIDv7). Server-stamped on create.
//   - Name is a stable DNS-1123 slug, unique-per-kind, mutable via the
//     id-routed update path. Used in URLs and YAML refs.
//   - DisplayName is free text shown in UI. Edits are free; nothing
//     references it.
//   - Description is free text documenting the row.
//   - Owner identifies provenance.
//   - Labels are arbitrary k/v selectors.
//   - CreatedAt/UpdatedAt are server-derived, read-only provenance read
//     from the row's dedicated columns (NOT the metadata JSONB — see
//     MarshalJSONB, which serializes only Description/Owner/Labels). They
//     are omitted from YAML manifests (yaml:"-") since seed/apply must not
//     set them; the store stamps them on read.
type Metadata struct {
	ID          string            `json:"id,omitempty"          yaml:"id,omitempty"          validate:"omitempty,uuid"`
	Name        string            `json:"name"                  yaml:"name"                  validate:"required,slug"`
	DisplayName string            `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Owner       Owner             `json:"owner,omitempty"       yaml:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"      yaml:"labels,omitempty"`
	CreatedAt   time.Time         `json:"createdAt,omitempty"   yaml:"-"`
	UpdatedAt   time.Time         `json:"updatedAt,omitempty"   yaml:"-"`
}

// NewID returns a fresh UUIDv7 string. Centralized so every entity store
// stamps ids the same way.
func NewID() string { return ids.New() }

// Owner describes who created / manages a row. Kind=provider requires ID;
// the other kinds leave ID empty.
type Owner struct {
	Kind OwnerKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	ID   string    `json:"id,omitempty"   yaml:"id,omitempty"`
}

// Icon describes a visual asset for a catalog entity (Provider, Host).
// Path is relative — typically "/provider/anthropic.svg" — and the
// frontend prefixes its own asset-base URL at render time. Relay does
// not serve the asset; this field is operator metadata.
type Icon struct {
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// OwnerKind enumerates provenance categories.
type OwnerKind string

const (
	OwnerSystem   OwnerKind = "system"
	OwnerProvider OwnerKind = "provider"
	OwnerHost     OwnerKind = "host"
	OwnerUser     OwnerKind = "user"
)
