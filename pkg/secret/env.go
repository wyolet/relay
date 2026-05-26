package secret

import (
	"context"
	"fmt"
	"os"
)

var _ Resolver = EnvResolver{}

// EnvResolver resolves KindEnv refs from the process environment. Pure —
// no persistence, no master key. An unset or empty variable is a loud
// error: a secret that silently resolves empty is worse than a failure.
type EnvResolver struct{}

func (EnvResolver) Resolve(_ context.Context, ref Ref) ([]byte, error) {
	if ref.Kind != KindEnv {
		return nil, fmt.Errorf("secret/env: wrong kind %q", ref.Kind)
	}
	v, ok := os.LookupEnv(ref.Env)
	if !ok || v == "" {
		return nil, fmt.Errorf("secret/env: %q is unset or empty", ref.Env)
	}
	return []byte(v), nil
}
