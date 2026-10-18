package stored

import "context"

// ScopeSystem is the scope of relay's own secrets (the token signing key, payload-log credentials) and of host keys the deployment owns.
const ScopeSystem = "system"

// OwnerScope is the scope a host key's stored value is sealed under: its owner, so each user's or project's keys sit under their own data key.
func OwnerScope(kind, id string) string {
	if kind == "" || kind == ScopeSystem {
		return ScopeSystem
	}
	return kind + ":" + id
}

type scopeKey struct{}

// WithScope makes stored secrets resolved under ctx decrypt only under scope, instead of the scope recorded on the row, so a row whose scope column was edited or missed a rescope loads unresolved. Resolvers that wrap the stored backend (OAuth) pass it through untouched.
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

func scopeFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(scopeKey{}).(string)
	return s, ok
}

// valueAAD binds a sealed value to its row, so ciphertext copied to another row does not open.
func valueAAD(id string) []byte { return []byte("relay/secret_values/" + id) }
