package settings

import "github.com/wyolet/relay/pkg/secret"

// SecretRefs returns the secret refs a section value holds, keyed by field
// path, so deleting a stored secret can tell whether a section still names
// it. A section with a secret.Ref field must be listed here.
func SecretRefs(v any) map[string]secret.Ref {
	switch s := v.(type) {
	case *AuthTokens:
		return map[string]secret.Ref{
			"signingKey":         s.SigningKey,
			"previousSigningKey": s.PreviousSigningKey,
		}
	case *PayloadLogging:
		return map[string]secret.Ref{
			"s3.accessKey": s.S3.AccessKey,
			"s3.secretKey": s.S3.SecretKey,
		}
	}
	return nil
}
