package telemetry

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Standard OpenTelemetry environment variables, read first so the emitter works against any OTLP backend.
const (
	EnvOTLPEndpoint   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPHeaders    = "OTEL_EXPORTER_OTLP_HEADERS"
	EnvServiceName    = "OTEL_SERVICE_NAME"
	EnvCaptureContent = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"
)

// Relay's environment variables, the fallback when no OTLP endpoint is set: the receiver is at WR_BASE_URL + "/otlp" and takes the relay key.
const (
	EnvRelayBaseURL = "WR_BASE_URL"
	EnvRelayAPIKey  = "WR_API_KEY"
)

// Defaults of the OpenTelemetry batch span processor.
const (
	DefaultQueueSize     = 2048
	DefaultBatchSize     = 512
	DefaultBatchInterval = 5 * time.Second
)

// defaultExportTimeout bounds one export request, the OTLP exporter default.
const defaultExportTimeout = 10 * time.Second

const sdkModule = "github.com/wyolet/relay/sdk"

// Option configures an Emitter. Explicit options win over the environment.
type Option func(*config)

type config struct {
	endpoint      string
	apiKey        string
	headers       map[string]string
	content       *bool
	serviceName   string
	http          *http.Client
	queueSize     int
	batchSize     int
	batchInterval time.Duration
}

// WithEndpoint sets the OTLP/HTTP base URL; "/v1/traces" and "/v1/logs" are appended. For relay it is the inference URL plus "/otlp".
func WithEndpoint(url string) Option { return func(c *config) { c.endpoint = url } }

// WithAPIKey sends key as "Authorization: Bearer <key>", the relay key for relay's receiver.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithHeader sets one header sent on every export.
func WithHeader(k, v string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[k] = v
	}
}

// WithContent sets the client's opt-in to sending message content. Relay may override it either way; see Emitter.SendsContent.
func WithContent(on bool) Option { return func(c *config) { c.content = &on } }

// WithServiceName sets the service.name resource attribute.
func WithServiceName(name string) Option { return func(c *config) { c.serviceName = name } }

// WithHTTPClient sets the client exports are sent with.
func WithHTTPClient(h *http.Client) Option { return func(c *config) { c.http = h } }

// WithQueueSize sets how many recorded calls may wait for export before new ones are dropped.
func WithQueueSize(n int) Option { return func(c *config) { c.queueSize = n } }

// WithBatch sets the most calls one export carries and how long a call may wait for its batch to fill.
func WithBatch(size int, interval time.Duration) Option {
	return func(c *config) { c.batchSize, c.batchInterval = size, interval }
}

// resolve fills what the options left unset from the environment and reports missing or malformed configuration.
func (c *config) resolve() error {
	var errs []error
	fromRelayEnv := false
	switch {
	case c.endpoint != "":
	case os.Getenv(EnvOTLPEndpoint) != "":
		c.endpoint = os.Getenv(EnvOTLPEndpoint)
	case os.Getenv(EnvRelayBaseURL) != "":
		c.endpoint = strings.TrimRight(os.Getenv(EnvRelayBaseURL), "/") + "/otlp"
		fromRelayEnv = true
	default:
		errs = append(errs, fmt.Errorf("telemetry: missing config — set %s or %s, or pass WithEndpoint", EnvOTLPEndpoint, EnvRelayBaseURL))
	}
	c.endpoint = strings.TrimRight(c.endpoint, "/")

	headers, err := parseOTLPHeaders(os.Getenv(EnvOTLPHeaders))
	if err != nil {
		errs = append(errs, err)
	}
	for k, v := range c.headers {
		headers[k] = v
	}
	c.headers = headers
	// The relay key goes only to the endpoint WR_BASE_URL named, never to a backend configured through the OTLP variables.
	if c.apiKey == "" && fromRelayEnv {
		if c.apiKey = os.Getenv(EnvRelayAPIKey); c.apiKey == "" && !hasHeader(headers, "Authorization") {
			errs = append(errs, fmt.Errorf("telemetry: missing config — set %s for the relay at %s", EnvRelayAPIKey, EnvRelayBaseURL))
		}
	}
	if c.apiKey != "" {
		for k := range c.headers {
			if strings.EqualFold(k, "Authorization") {
				delete(c.headers, k)
			}
		}
		c.headers["Authorization"] = "Bearer " + c.apiKey
	}

	if c.content == nil {
		on := captureContentEnv(os.Getenv(EnvCaptureContent))
		c.content = &on
	}
	if c.serviceName == "" {
		if c.serviceName = os.Getenv(EnvServiceName); c.serviceName == "" {
			c.serviceName = "unknown_service:" + filepath.Base(os.Args[0])
		}
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultExportTimeout}
	}
	if c.queueSize <= 0 {
		c.queueSize = DefaultQueueSize
	}
	if c.batchSize <= 0 {
		c.batchSize = DefaultBatchSize
	}
	if c.batchInterval <= 0 {
		c.batchInterval = DefaultBatchInterval
	}
	return errors.Join(errs...)
}

// captureContentEnv reads the variable the OpenTelemetry GenAI instrumentations use. Every value that sends content somewhere turns it on, since this emitter carries content on the event only.
func captureContentEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "span_only", "event_only", "span_and_event":
		return true
	}
	return false
}

// parseOTLPHeaders reads "k1=v1,k2=v2" with percent-encoded values, the OTLP exporter format.
func parseOTLPHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return out, fmt.Errorf("telemetry: invalid %s entry %q (want k=v)", EnvOTLPHeaders, pair)
		}
		value, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return out, fmt.Errorf("telemetry: invalid %s value for %q: %w", EnvOTLPHeaders, k, err)
		}
		out[k] = value
	}
	return out, nil
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// sdkVersion is the SDK module's version from the build information, "" when the build does not record one (a workspace build).
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if info.Main.Path == sdkModule && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, d := range info.Deps {
		if d.Path == sdkModule {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return ""
}
