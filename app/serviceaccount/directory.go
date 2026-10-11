package serviceaccount

import (
	"encoding/json"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

// policyIDKey is the spec field the policy override is stored under, as it was before the directory held these rows.
const policyIDKey = "policyId"

// PolicyIDAttribute reads the policy override out of a directory row's attributes; "" when the row has none.
func PolicyIDAttribute(attrs map[string]json.RawMessage) (string, error) {
	var id string
	err := meta.Attribute(attrs, policyIDKey, &id)
	return id, err
}

// WithPolicyID returns attrs carrying id; "" leaves the override out.
func WithPolicyID(attrs map[string]json.RawMessage, id string) (map[string]json.RawMessage, error) {
	return meta.PutAttribute(attrs, policyIDKey, id, id != "")
}

// ToDirectory is s as the directory stores it.
func (s *ServiceAccount) ToDirectory() (*directory.ServiceAccount, error) {
	attrs, err := WithPolicyID(nil, s.Spec.PolicyID)
	if err != nil {
		return nil, err
	}
	return &directory.ServiceAccount{
		Meta: meta.ToDirectory(s.Meta),
		Spec: directory.ServiceAccountSpec{ProjectID: s.Spec.ProjectID, Enabled: s.Spec.Enabled, Attributes: attrs},
	}, nil
}

// FromDirectory is a directory row as a ServiceAccount.
func FromDirectory(r *directory.ServiceAccount) (*ServiceAccount, error) {
	policyID, err := PolicyIDAttribute(r.Spec.Attributes)
	if err != nil {
		return nil, err
	}
	sa := &ServiceAccount{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{ProjectID: r.Spec.ProjectID, PolicyID: policyID, Enabled: r.Spec.Enabled},
	}
	sa.StampOwner()
	return sa, nil
}
