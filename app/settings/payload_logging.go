package settings

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/pkg/secret"
)

// SectionPayloadLogging is the section key for the request/response body
// capture observer's runtime config. Mutable at runtime via the settings
// API — the payload observer reconciles the live value and hot-swaps its
// sink (toggle, backend, bucket, credentials) without a restart.
const SectionPayloadLogging = "payload-logging"

// PayloadLogging configures the payloadlog observer. Off by default; the
// per-request opt-in (policy/key) still gates capture on top of
// Enabled. Credentials are secret.Refs resolved via pkg/secret, so they
// can live in env, encrypted-PG, or a future external backend — never as
// plaintext in this row.
type PayloadLogging struct {
	// Enabled is the global master switch. When false the observer
	// produces nothing regardless of per-request opt-in.
	Enabled bool `json:"enabled"`

	// Backend selects the sink: "file" (default), "s3", or "clickhouse".
	Backend string `json:"backend"`

	// MaxBytes caps each stored body, measured on the DECODED plaintext
	// (compressed upstream bodies are decoded before capture); 0 = unlimited.
	MaxBytes int `json:"maxBytes"`

	// RetentionDays bounds how long captured bodies are kept by the
	// clickhouse backend; 0 keeps them forever. A stored row without the
	// field decodes to DefaultPayloadRetentionDays, not to keep-forever.
	RetentionDays int `json:"retentionDays"`

	File PayloadFile       `json:"file"`
	S3   PayloadS3         `json:"s3"`
	CH   PayloadClickHouse `json:"clickhouse"`
}

// PayloadFile configures the JSONL file backend.
type PayloadFile struct {
	Path string `json:"path"`
}

// PayloadS3 configures the object-store backend. AccessKey/SecretKey are
// secret.Refs resolved at sink-build time: env names under RELAY_PAYLOAD_S3_
// or stored ids under "payload-logging:".
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
	// WALDir overrides the local WAL segment directory; empty uses the
	// boot default.
	WALDir string `json:"walDir,omitempty"`

	// Dedup stores each distinct message, tool list and system prompt once
	// and request bodies as references to them. Rows written either way
	// stay readable.
	Dedup bool `json:"dedup,omitempty"`
}

// Validate is enforced before any write. Only meaningful when Enabled —
// a disabled section can hold partial config (e.g. while an operator
// fills in S3 details before flipping it on). The S3 credentials and
// transport are checked regardless: the log reader builds from this
// section whatever the toggle says.
func (p *PayloadLogging) Validate() error {
	if p.RetentionDays < 0 {
		return fmt.Errorf("payload-logging: retentionDays must be >= 0")
	}
	if err := validateOptionalRef("s3.accessKey", p.S3.AccessKey); err != nil {
		return err
	}
	if err := validateOptionalRef("s3.secretKey", p.S3.SecretKey); err != nil {
		return err
	}
	if p.Backend == "s3" && !p.S3.UseSSL && !plainHTTPAllowed() {
		return fmt.Errorf("payload-logging: s3.useSSL: %w: must be true (false needs RELAY_COOKIE_SECURE=false)", ErrSecretRefNotAllowed)
	}
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
	case "clickhouse":
		// DSN comes from the boot CH config (RELAY_CH_DSN), validated there;
		// the operator is responsible for ensuring it's set.
	default:
		return fmt.Errorf("payload-logging: backend must be \"file\", \"s3\", or \"clickhouse\", got %q", p.Backend)
	}
	if p.MaxBytes < 0 {
		return fmt.Errorf("payload-logging: maxBytes must be >= 0")
	}
	return nil
}

// DefaultPayloadRetentionDays applies when payload-logging sets no
// retentionDays.
const DefaultPayloadRetentionDays = 30

// decodePayloadLogging unmarshals onto the default retention, so a field
// absent from the stored row keeps it.
func decodePayloadLogging(raw []byte) (any, error) {
	v := PayloadLogging{RetentionDays: DefaultPayloadRetentionDays}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
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
	return checkSecretRef(SectionPayloadLogging, field, r)
}

func init() {
	Register(Section{
		Name:        SectionPayloadLogging,
		Description: "Request/response body capture sink config (toggle, backend file|s3|clickhouse, size cap, retention, S3 settings with secret-ref credentials, ClickHouse WAL override and dedup). retentionDays applies to clickhouse (0 = keep forever, absent = 30; shortening deletes older bodies). clickhouse.dedup (default false) stores request bodies as references to shared pieces, written once; pair it with maxBytes 0. Hot-reloaded — changes take effect without a restart.",
		Defaults: func() any {
			// 4 MiB default: a large model response (long output + thinking)
			// clears 1 MiB of plaintext easily, and the cap applies to the
			// DECODED body. 0 disables the cap — reasonable when the backend
			// is s3; inline backends (file/clickhouse) should keep one.
			return &PayloadLogging{Backend: "file", MaxBytes: 4 << 20, RetentionDays: DefaultPayloadRetentionDays, File: PayloadFile{Path: "relay-payloads.jsonl"}}
		},
		Decode: decodePayloadLogging,
	})
}
