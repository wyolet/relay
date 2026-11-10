// The tenancy and RBAC kinds are stored by the auth module's directory; these convert the envelope and the product-only spec fields, which the module keeps as opaque attributes.

package meta

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is m as the directory stores it.
func ToDirectory(m Metadata) directory.Metadata {
	return directory.Metadata{
		ID: m.ID, Name: m.Name, DisplayName: m.DisplayName, Description: m.Description,
		Owner:  authz.Owner{Kind: string(m.Owner.Kind), ID: m.Owner.ID},
		Labels: m.Labels, Annotations: m.Annotations, Dirty: m.Dirty,
		ResourceVersion: m.ResourceVersion, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// FromDirectory is the directory's metadata as the relay carries it.
func FromDirectory(m directory.Metadata) Metadata {
	return Metadata{
		ID: m.ID, Name: m.Name, DisplayName: m.DisplayName, Description: m.Description,
		Owner:  Owner{Kind: OwnerKind(m.Owner.Kind), ID: m.Owner.ID},
		Labels: m.Labels, Annotations: m.Annotations, Dirty: m.Dirty,
		ResourceVersion: m.ResourceVersion, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// PutAttribute stores v under key, or leaves the key out when set is false, as the relay's omitempty spec fields did.
func PutAttribute(attrs map[string]json.RawMessage, key string, v any, set bool) (map[string]json.RawMessage, error) {
	if !set {
		return attrs, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("spec.%s: %w", key, err)
	}
	if attrs == nil {
		attrs = map[string]json.RawMessage{}
	}
	attrs[key] = raw
	return attrs, nil
}

// Attribute decodes the attribute under key into dst; a missing key leaves dst untouched.
func Attribute(attrs map[string]json.RawMessage, key string, dst any) error {
	raw, ok := attrs[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("spec.%s: %w", key, err)
	}
	return nil
}

// FromDirectoryError maps the directory's stale-version error onto ErrStaleResourceVersion, which the control API answers with 409.
func FromDirectoryError(err error) error {
	if errors.Is(err, directory.ErrStaleResourceVersion) {
		return ErrStaleResourceVersion
	}
	return err
}
