package settings

import (
	"fmt"

	"github.com/wyolet/relay/pkg/secret"
)

// SectionPayloadLogging is the section key for the request/response body
// capture observer's runtime config. Mutable at runtime via the settings
// API — the payload observer reconciles the live value and hot-swaps its
// sink (toggle, backend, bucket, credentials) without a restart.
const SectionPayloadLogging = "payload-logging"

// PayloadLogging configures the payloadlog observer. Off by default; the
// per-request opt-in (policy/relaykey) still gates capture on top of
// Enabled. Credentials are secret.Refs resolved via pkg/secret, so they
// can live in env, encrypted-PG, or a future external backend — never as
// plaintext in this row.
type PayloadLogging struct {
	// Enabled is the global master switch. When false the observer
	// produces nothing regardless of per-request opt-in.
	Enabled bool `json:"enabled"`

	// Backend selects the sink: "file" (default), "s3", or "clickhouse".
	Backend string `json:"backend"`

	// MaxBytes caps each stored body; 0 = unlimited.
	MaxBytes int `json:"maxBytes"`

	File PayloadFile       `json:"file"`
	S3   PayloadS3         `json:"s3"`
	CH   PayloadClickHouse `json:"clickhouse"`
}

// PayloadFile configures the JSONL file backend.
type PayloadFile struct {
	Path string `json:"path"`
}

// PayloadS3 configures the object-store backend. AccessKey/SecretKey are
// secret.Refs (kind env or stored), resolved at sink-build time.
type PayloadS3 struct {
	Endpoint  string     `json:"endpoint"`
	Bucket    string     `json:"bucket"`
	Region    string     `json:"region,omitempty"`
	Prefix    string     `json:"prefix,omitempty"`
	UseSSL    bool       `json:"useSSL"`
	AccessKey secret.Ref `json:"accessKey"`
	SecretKey secret.Ref `json:"secretKey"`
}

// PayloadClickHouse configures the ClickHouse backend (Langfuse-style: text
// bodies as ZSTD String columns, queryable). The DSN is NOT stored here — it
// reuses the relay's boot CH connection (RELAY_CH_DSN, the same cluster the
// usage sink uses), so no credentials live in this row. Only the per-backend
// knobs that are safe to hot-swap live here.
type PayloadClickHouse struct {
	// RetentionDays overrides the MergeTree TTL; 0 uses the backend default.
	RetentionDays int `json:"retentionDays,omitempty"`
	// WALDir overrides the local WAL segment directory; empty uses the
	// boot default.
	WALDir string `json:"walDir,omitempty"`
}

// Validate is enforced before any write. Only meaningful when Enabled —
// a disabled section can hold partial config (e.g. while an operator
// fills in S3 details before flipping it on).
func (p *PayloadLogging) Validate() error {
	if !p.Enabled {
		return nil
	}
	switch p.Backend {
	case "", "file":
		// file is the default; Path empty falls back at build time.
	case "s3":
		if p.S3.Bucket == "" {
			return fmt.Errorf("payload-logging: s3 backend requires s3.bucket")
		}
		if err := validateOptionalRef("s3.accessKey", p.S3.AccessKey); err != nil {
			return err
		}
		if err := validateOptionalRef("s3.secretKey", p.S3.SecretKey); err != nil {
			return err
		}
	case "clickhouse":
		// DSN comes from the boot CH config (RELAY_CH_DSN), validated there;
		// the operator is responsible for ensuring it's set. RetentionDays/
		// WALDir are optional overrides with backend defaults.
		if p.CH.RetentionDays < 0 {
			return fmt.Errorf("payload-logging: clickhouse.retentionDays must be >= 0")
		}
	default:
		return fmt.Errorf("payload-logging: backend must be \"file\", \"s3\", or \"clickhouse\", got %q", p.Backend)
	}
	if p.MaxBytes < 0 {
		return fmt.Errorf("payload-logging: maxBytes must be >= 0")
	}
	return nil
}

// validateOptionalRef allows an empty (zero) Ref — some S3 deployments use
// ambient credentials (IAM role) — but a partially-filled Ref must be
// valid.
func validateOptionalRef(field string, r secret.Ref) error {
	if r.Kind == "" && r.Env == "" && r.ID == "" {
		return nil
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("payload-logging: %s: %w", field, err)
	}
	return nil
}

func init() {
	Register(Section{
		Name:        SectionPayloadLogging,
		Description: "Request/response body capture sink config (toggle, backend file|s3|clickhouse, size cap, S3 settings with secret-ref credentials, ClickHouse retention/WAL overrides). Hot-reloaded — changes take effect without a restart.",
		Defaults: func() any {
			return &PayloadLogging{Backend: "file", MaxBytes: 1 << 20, File: PayloadFile{Path: "relay-payloads.jsonl"}}
		},
		Decode: decodeAndValidate[PayloadLogging, *PayloadLogging],
	})
}
