package manifest

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// ---------------------------------------------------------------------------
// HostKey
// ---------------------------------------------------------------------------

// ToHostKey resolves Spec.HostID and Spec.PolicyID (name → id).
func ToHostKey(d HostKeyDTO, idx Resolver) (*hostkey.HostKey, error) {
	m := d.Metadata.toMeta()
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	resolveScopeOwner(&m.Owner, idx)
	hostID := d.Spec.HostID
	if hostID != "" {
		if id, ok := idx.HostID(hostID); ok {
			hostID = id
		}
	}
	policyID := d.Spec.PolicyID
	if policyID != "" {
		if id, ok := idx.PolicyID(policyID); ok {
			policyID = id
		}
	}
	return &hostkey.HostKey{
		Meta: m,
		Spec: hostkey.Spec{
			HostID:   hostID,
			PolicyID: policyID,
			ValueFrom: hostkey.ValueFrom{
				Kind:     hostkey.ValueKind(d.Spec.ValueFrom.Kind),
				Env:      d.Spec.ValueFrom.Env,
				Provider: d.Spec.ValueFrom.Provider,
			},
			DefaultTier:     d.Spec.DefaultTier,
			PricingStrategy: d.Spec.PricingStrategy,
			Enabled:         d.Spec.Enabled,
			Value:           d.Spec.Value,
		},
	}, nil
}

func FromHostKey(k *hostkey.HostKey, rev ReverseResolver) HostKeyDTO {
	wm := metaToWire(k.Meta)
	hostID := k.Spec.HostID
	if hostID != "" {
		if hname, ok := rev.HostName(hostID); ok {
			hostID = hname
		}
	}
	policyID := k.Spec.PolicyID
	if policyID != "" {
		if pname, ok := rev.PolicyName(policyID); ok {
			policyID = pname
		}
	}
	return HostKeyDTO{
		APIVersion: APIVersion,
		Kind:       "HostKey",
		Metadata:   wm,
		Spec: HostKeySpec{
			HostID:   hostID,
			PolicyID: policyID,
			ValueFrom: HostKeyValueFrom{
				Kind:     string(k.Spec.ValueFrom.Kind),
				Env:      k.Spec.ValueFrom.Env,
				Provider: k.Spec.ValueFrom.Provider,
			},
			DefaultTier:     k.Spec.DefaultTier,
			PricingStrategy: k.Spec.PricingStrategy,
			Enabled:         k.Spec.Enabled,
			// Value intentionally omitted — never returned in responses
		},
	}
}

// ---------------------------------------------------------------------------
// Key
// ---------------------------------------------------------------------------

func ToKey(d KeyDTO, idx Resolver) (*key.Key, error) {
	// A Key without a policy resolves through its principal instead
	// (ServiceAccount.policy, then the policy bindings), so the field is
	// optional on the wire as it is in the domain.
	var policyID string
	if d.Spec.Policy != "" {
		id, ok := idx.PolicyID(d.Spec.Policy)
		if !ok {
			return nil, refNotFound("key %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
		}
		policyID = id
	}

	principal, err := toPrincipal(d.Metadata.Name, d.Spec.Principal, idx)
	if err != nil {
		return nil, err
	}

	expiresAt, err := parseOptionalTime(d.Metadata.Name, "expiresAt", d.Spec.ExpiresAt)
	if err != nil {
		return nil, err
	}
	revokedAt, err := parseOptionalTime(d.Metadata.Name, "revokedAt", d.Spec.RevokedAt)
	if err != nil {
		return nil, err
	}

	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	return &key.Key{
		Meta: m,
		Spec: key.Spec{
			Principal:             principal,
			PolicyID:              policyID,
			KeyHash:               d.Spec.KeyHash,
			Prefix:                d.Spec.Prefix,
			ExpiresAt:             expiresAt,
			RevokedAt:             revokedAt,
			Enabled:               d.Spec.Enabled,
			PassthroughAllowed:    d.Spec.PassthroughAllowed,
			PayloadLoggingEnabled: d.Spec.PayloadLoggingEnabled,
		},
	}, nil
}

// toPrincipal resolves the wire principal name to an id: a service account
// slug or a username, per kind.
func toPrincipal(keyName string, p PrincipalDTO, idx Resolver) (key.Principal, error) {
	switch key.PrincipalKind(p.Kind) {
	case key.PrincipalServiceAccount:
		id, ok := idx.ServiceAccountID(p.Name)
		if !ok {
			return key.Principal{}, refNotFound("key %q: service account %q not found", keyName, p.Name)
		}
		return key.Principal{Kind: key.PrincipalServiceAccount, ID: id}, nil
	case key.PrincipalUser:
		id, ok := idx.UserID(p.Name)
		if !ok {
			return key.Principal{}, refNotFound("key %q: user %q not found", keyName, p.Name)
		}
		return key.Principal{Kind: key.PrincipalUser, ID: id}, nil
	default:
		return key.Principal{}, fmt.Errorf("key %q: principal.kind must be serviceaccount or user, got %q", keyName, p.Kind)
	}
}

func parseOptionalTime(name, field string, raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, fmt.Errorf("key %q: %s: %w", name, field, err)
	}
	return &t, nil
}

func FromKey(k *key.Key, rev ReverseResolver) KeyDTO {
	policyName, _ := rev.PolicyName(k.Spec.PolicyID)
	if policyName == "" {
		policyName = k.Spec.PolicyID
	}

	principal := PrincipalDTO{Kind: string(k.Spec.Principal.Kind), Name: k.Spec.Principal.ID}
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		if n, ok := rev.ServiceAccountName(k.Spec.Principal.ID); ok {
			principal.Name = n
		}
	case key.PrincipalUser:
		if n, ok := rev.Username(k.Spec.Principal.ID); ok {
			principal.Name = n
		}
	}

	return KeyDTO{
		APIVersion: APIVersion,
		Kind:       "Key",
		Metadata:   metaToWire(k.Meta),
		Spec: KeySpec{
			Principal:             principal,
			Policy:                policyName,
			KeyHash:               k.Spec.KeyHash,
			Prefix:                k.Spec.Prefix,
			ExpiresAt:             formatOptionalTime(k.Spec.ExpiresAt),
			RevokedAt:             formatOptionalTime(k.Spec.RevokedAt),
			Enabled:               k.Spec.Enabled,
			PassthroughAllowed:    k.Spec.PassthroughAllowed,
			PayloadLoggingEnabled: k.Spec.PayloadLoggingEnabled,
		},
	}
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
