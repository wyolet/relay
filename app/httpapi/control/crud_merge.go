package control

import "github.com/wyolet/relay/app/hostkey"

// mergeHostKeyPreserveValue treats an empty Spec.Value on a stored- or
// oauth-mode update as "keep the existing credential" — the caller wants to
// edit metadata or rebind to a different policy/host without rotating it. A
// non-empty Value still means rotation. Env-mode keys carry no value here, so
// this is a no-op for them.
func mergeHostKeyPreserveValue(existing, incoming *hostkey.HostKey) {
	if existing == nil || incoming == nil {
		return
	}
	if incoming.Spec.Value != "" {
		return // explicit new value → rotation
	}
	switch incoming.Spec.ValueFrom.Kind {
	case hostkey.ValueKindStored:
		// Re-supply the existing secret so the store re-encrypts it unchanged.
		incoming.Spec.Value = existing.Resolved
	case hostkey.ValueKindOAuth:
		// existing.Resolved is the access token, NOT the stored token blob, so
		// it can't be re-encrypted as the value. Carry Resolved so Validate
		// passes; the store preserves the existing blob ciphertext as-is.
		incoming.Resolved = existing.Resolved
	}
}
