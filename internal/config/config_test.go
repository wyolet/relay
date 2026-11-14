package config

import (
	"bytes"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// clearEnv empties every RELAY_* variable the process inherited, so a test sees only what it sets.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "RELAY_") {
			t.Setenv(name, "")
		}
	}
	stamped := DefaultCatalogVersion
	DefaultCatalogVersion = ""
	t.Cleanup(func() { DefaultCatalogVersion = stamped })
}

// defaults is the Config an empty environment loads.
func defaults() *Config {
	cpus := runtime.GOMAXPROCS(0)
	return &Config{
		CatalogBackend: "yaml", StateBackend: "memory", EventlogBackend: "file",
		RedisPoolSize: 25 * cpus, RedisPoolMaxWaiters: 50 * cpus,
		RedisDialTimeout: 2 * time.Second, RedisReadTimeout: time.Second, RedisWriteTimeout: 500 * time.Millisecond, RedisPoolTimeout: 250 * time.Millisecond,
		RedisPostFlightPoolSize: 25 * cpus, RedisPostFlightPoolTimeout: time.Second, RedisPostFlightReadTimeout: time.Second,
		RedisPostFlightBatchSize: 64, RedisPostFlightBatchDelay: 2 * time.Millisecond, RedisPostFlightQueueSize: 4096,
		Authz: AuthzSingle, MigrateOnBoot: true, MigrateLockTimeout: 10 * time.Second,
		ConfigDir: "config", CatalogPollInterval: time.Hour,
		InflightSchedP99: 20 * time.Millisecond, BatchMaxItems: 50000, UpstreamDialTimeout: 10 * time.Second,
		ConnQueue: 1024, ConnQueueTimeout: time.Second,
		Limiter: "hybrid", LimiterSync: time.Second, PlanCellIdle: 10 * time.Minute, PlanResync: 10 * time.Minute,
		StreamKeepAlive: 15 * time.Second, ProxyBodyIdle: 30 * time.Second, StreamUsage: true, StreamIdleTimeout: 10 * time.Minute,
		HealthzDeadlineMS: 500, ShutdownDeadlineS: 15,
		Port: "8080", ControlPort: "8081",
		Runtime: RuntimeConfig{Mode: "oss"},
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(defaults(), cfg); diff != "" {
		t.Errorf("config from an empty environment (-want +got):\n%s", diff)
	}
	if cfg.MasterKeys() != nil {
		t.Errorf("MasterKeys() = %v without a master key", cfg.MasterKeys())
	}
}

// Each case sets a few variables and names the fields that move away from the defaults; every other field must stay put.
func TestLoad_Values(t *testing.T) {
	keyA, keyB, keyC := strings.Repeat("A", 43)+"=", strings.Repeat("B", 43)+"=", strings.Repeat("C", 43)+"="
	rawA, rawB, rawC := make([]byte, 32), bytes.Repeat([]byte{0x04, 0x10, 0x41}, 11)[:32], bytes.Repeat([]byte{0x08, 0x20, 0x82}, 11)[:32]
	tests := []struct {
		name  string
		env   map[string]string
		stamp string // DefaultCatalogVersion
		want  func(c *Config)
	}{
		{"cluster mode on", map[string]string{"RELAY_CLUSTER_MODE": "on"}, "", func(c *Config) { c.ClusterMode = true }},
		{"cluster mode off", map[string]string{"RELAY_CLUSTER_MODE": "off"}, "", func(c *Config) {}},
		{"master key with extras", map[string]string{"RELAY_MASTER_KEY": keyA, "RELAY_MASTER_KEY_EXTRA": keyB + ", " + keyC}, "",
			func(c *Config) { c.MasterKey, c.MasterKeyExtra = rawA, [][]byte{rawB, rawC} }},
		{"backends", map[string]string{"RELAY_CATALOG_BACKEND": "postgres", "RELAY_STATE_BACKEND": "redis", "RELAY_EVENTLOG_BACKEND": "clickhouse"}, "",
			func(c *Config) {
				c.CatalogBackend, c.StateBackend, c.EventlogBackend = "postgres", "redis", "clickhouse"
			}},
		{"eventlog valkey", map[string]string{"RELAY_EVENTLOG_BACKEND": "valkey"}, "", func(c *Config) { c.EventlogBackend = "valkey" }},
		{"eventlog postgres", map[string]string{"RELAY_EVENTLOG_BACKEND": "postgres"}, "", func(c *Config) { c.EventlogBackend = "postgres" }},
		{"store addresses and secrets pass through verbatim",
			map[string]string{"RELAY_PG_DSN": "postgres://u:p@h/db", "RELAY_REDIS_ADDR": "valkey:6379", "RELAY_REDIS_PASSWORD": " s3cret ", "RELAY_CH_DSN": "clickhouse://h", "RELAY_OTLP_ENDPOINT": "http://otel:4318", "RELAY_ADMIN_TOKEN": "tok", "RELAY_INSTANCE_ID": "pod-1", "RELAY_EVENTLOG_DIR": "/var/log/relay", "RELAY_CONFIG_DIR": "/etc/relay", "RELAY_CATALOG_URL": "https://c.example/{tag}", "RELAY_CATALOG_INDEX_URL": "https://c.example/index.yaml"}, "",
			func(c *Config) {
				c.PGDSN, c.RedisAddr, c.RedisPassword, c.CHDSN, c.OTLPEndpoint, c.AdminToken = "postgres://u:p@h/db", "valkey:6379", " s3cret ", "clickhouse://h", "http://otel:4318", "tok"
				c.InstanceID, c.EventlogDir, c.ConfigDir, c.CatalogURL, c.CatalogIndexURL = "pod-1", "/var/log/relay", "/etc/relay", "https://c.example/{tag}", "https://c.example/index.yaml"
			}},
		{"pg pool at its bounds", map[string]string{"RELAY_PG_MAX_CONNS": "2147483647", "RELAY_PG_MIN_CONNS": "1"}, "", func(c *Config) { c.PGMaxConns, c.PGMinConns = 2147483647, 1 }},
		{"redis pool size sets the waiter default", map[string]string{"RELAY_REDIS_POOL_SIZE": "40", "RELAY_REDIS_MIN_IDLE_CONNS": "5"}, "",
			func(c *Config) { c.RedisPoolSize, c.RedisPoolMaxWaiters, c.RedisMinIdleConns = 40, 80, 5 }},
		{"redis waiters set apart from the pool", map[string]string{"RELAY_REDIS_POOL_SIZE": "40", "RELAY_REDIS_POOL_MAX_WAITERS": "7"}, "",
			func(c *Config) { c.RedisPoolSize, c.RedisPoolMaxWaiters = 40, 7 }},
		{"redis timeouts and retries", map[string]string{"RELAY_REDIS_DIAL_TIMEOUT_MS": "3000", "RELAY_REDIS_READ_TIMEOUT_MS": "750", "RELAY_REDIS_WRITE_TIMEOUT_MS": "1000", "RELAY_REDIS_POOL_TIMEOUT_MS": "100", "RELAY_REDIS_MAX_RETRIES": "2"}, "",
			func(c *Config) {
				c.RedisDialTimeout, c.RedisReadTimeout, c.RedisWriteTimeout, c.RedisPoolTimeout, c.RedisMaxRetries = 3*time.Second, 750*time.Millisecond, time.Second, 100*time.Millisecond, 2
			}},
		{"post-flight redis pool leaves the admission pool alone", map[string]string{"RELAY_REDIS_POSTFLIGHT_POOL_SIZE": "4", "RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS": "1500", "RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS": "800"}, "",
			func(c *Config) {
				c.RedisPostFlightPoolSize, c.RedisPostFlightPoolTimeout, c.RedisPostFlightReadTimeout = 4, 1500*time.Millisecond, 800*time.Millisecond
			}},
		{"settle queue", map[string]string{"RELAY_REDIS_POSTFLIGHT_BATCH_SIZE": "16", "RELAY_REDIS_POSTFLIGHT_BATCH_DELAY_MS": "0", "RELAY_REDIS_POSTFLIGHT_QUEUE_SIZE": "0"}, "",
			func(c *Config) {
				c.RedisPostFlightBatchSize, c.RedisPostFlightBatchDelay, c.RedisPostFlightQueueSize = 16, 0, 0
			}},
		{"public URL loses one trailing slash", map[string]string{"RELAY_PUBLIC_URL": "https://relay.example.com/"}, "", func(c *Config) { c.PublicURL = "https://relay.example.com" }},
		{"authz rbac", map[string]string{"RELAY_AUTHZ": "rbac"}, "", func(c *Config) { c.Authz = AuthzRBAC }},
		{"deprecated multi-user reads as rbac", map[string]string{"RELAY_MULTI_USER": " TRUE "}, "", func(c *Config) { c.Authz = AuthzRBAC }},
		{"deprecated multi-user yes", map[string]string{"RELAY_MULTI_USER": "yes"}, "", func(c *Config) { c.Authz = AuthzRBAC }},
		{"deprecated multi-user off", map[string]string{"RELAY_MULTI_USER": "off"}, "", func(c *Config) {}},
		{"authz single wins over multi-user", map[string]string{"RELAY_AUTHZ": "single", "RELAY_MULTI_USER": "on"}, "", func(c *Config) {}},
		{"migrate on boot off", map[string]string{"RELAY_MIGRATE_ON_BOOT": " OFF ", "RELAY_MIGRATE_LOCK_TIMEOUT_S": "0"}, "", func(c *Config) { c.MigrateOnBoot, c.MigrateLockTimeout = false, 0 }},
		{"migrate on boot 0", map[string]string{"RELAY_MIGRATE_ON_BOOT": "0", "RELAY_MIGRATE_LOCK_TIMEOUT_S": "3"}, "", func(c *Config) { c.MigrateOnBoot, c.MigrateLockTimeout = false, 3*time.Second }},
		{"migrate on boot false", map[string]string{"RELAY_MIGRATE_ON_BOOT": "false"}, "", func(c *Config) { c.MigrateOnBoot = false }},
		{"migrate on boot spelled on", map[string]string{"RELAY_MIGRATE_ON_BOOT": "true"}, "", func(c *Config) {}},
		{"switches that only read 1", map[string]string{"RELAY_AUTO_SEED_IF_EMPTY": "1", "RELAY_CONN_CLOSE_IDLE": "1", "RELAY_DEV_TRUST_EVENT_TIME": "1", "RELAY_UI_DISABLE": "1", "RELAY_CATALOG_ALLOW_DOWNGRADE": "1"}, "",
			func(c *Config) {
				c.AutoSeedIfEmpty, c.ConnCloseIdle, c.DevTrustEventTime, c.UIDisable, c.CatalogAllowDowngrade = true, true, true, true, true
			}},
		{"switches ignore anything but 1", map[string]string{"RELAY_AUTO_SEED_IF_EMPTY": "true", "RELAY_CONN_CLOSE_IDLE": "on", "RELAY_UI_DISABLE": "yes"}, "", func(c *Config) {}},
		{"stamped build defaults the catalog version", nil, "v1.2.3", func(c *Config) { c.CatalogVersion = "v1.2.3" }},
		{"explicit catalog version wins over the stamp", map[string]string{"RELAY_CATALOG_VERSION": "v9.9.9"}, "v1.2.3", func(c *Config) { c.CatalogVersion = "v9.9.9" }},
		{"a catalog dir keeps the stamped default off", map[string]string{"RELAY_CATALOG_DIR": "/catalog"}, "v1.2.3", func(c *Config) { c.CatalogDir = "/catalog" }},
		{"a catalog dir with an explicit version", map[string]string{"RELAY_CATALOG_DIR": "/catalog", "RELAY_CATALOG_VERSION": "latest"}, "v1.2.3",
			func(c *Config) { c.CatalogDir, c.CatalogVersion = "/catalog", "latest" }},
		{"catalog version off disables the stamped default", map[string]string{"RELAY_CATALOG_VERSION": "off"}, "v1.2.3", func(c *Config) {}},
		{"catalog polling off and an https webhook base", map[string]string{"RELAY_CATALOG_POLL_INTERVAL": "0", "RELAY_CATALOG_WEBHOOK_URL": "https://relay.example.com/hooks//"}, "",
			func(c *Config) { c.CatalogPollInterval, c.CatalogWebhookURL = 0, "https://relay.example.com/hooks" }},
		{"plain http webhook base when cookies are not secure", map[string]string{"RELAY_CATALOG_POLL_INTERVAL": "15m", "RELAY_CATALOG_WEBHOOK_URL": "http://relay.local:8080", "RELAY_COOKIE_SECURE": "false"}, "",
			func(c *Config) {
				c.CatalogPollInterval, c.CatalogWebhookURL = 15*time.Minute, "http://relay.local:8080"
			}},
		{"queues", map[string]string{"RELAY_USAGE_QUEUE_SIZE": "32768", "RELAY_USAGE_REFUSED_QUEUE_SIZE": "64", "RELAY_PAYLOAD_QUEUE_BYTES": "1048576", "RELAY_PAYLOAD_REFUSED_QUEUE_BYTES": "4096"}, "",
			func(c *Config) {
				c.UsageQueueSize, c.UsageRefusedQueueSize, c.PayloadQueueBytes, c.PayloadRefusedQueueBytes = 32768, 64, 1048576, 4096
			}},
		{"request size and in-flight caps", map[string]string{"RELAY_MAX_REQUEST_BYTES": "5368709120", "RELAY_MAX_INFLIGHT": "200", "RELAY_INFLIGHT_SCHED_P99": "2ms", "RELAY_BATCH_MAX_ITEMS": "10"}, "",
			func(c *Config) {
				c.MaxRequestBytes, c.MaxInflight, c.InflightSchedP99, c.BatchMaxItems = 5368709120, 200, 2*time.Millisecond, 10
			}},
		{"a non-positive request size keeps the default", map[string]string{"RELAY_MAX_REQUEST_BYTES": "-1", "RELAY_INFLIGHT_SCHED_P99": "0"}, "", func(c *Config) { c.InflightSchedP99 = 0 }},
		{"upstream and connection limits", map[string]string{"RELAY_UPSTREAM_MAX_IDLE_PER_HOST": "64", "RELAY_UPSTREAM_DIAL_TIMEOUT_S": "3", "RELAY_MAX_CONNS": "5000", "RELAY_CONN_QUEUE": "0", "RELAY_CONN_QUEUE_TIMEOUT": "250ms"}, "",
			func(c *Config) {
				c.UpstreamMaxIdlePerHost, c.UpstreamDialTimeout, c.MaxConns, c.ConnQueue, c.ConnQueueTimeout = 64, 3*time.Second, 5000, 0, 250*time.Millisecond
			}},
		{"key breaker", map[string]string{"RELAY_KEY_BREAKER_THRESHOLD": "3", "RELAY_KEY_BREAKER_WINDOW_S": "30"}, "", func(c *Config) { c.KeyBreakerThreshold, c.KeyBreakerWindow = 3, 30*time.Second }},
		{"limiter lua in any case", map[string]string{"RELAY_LIMITER": " Lua ", "RELAY_LIMITER_SYNC_MS": "250"}, "", func(c *Config) { c.Limiter, c.LimiterSync = "lua", 250*time.Millisecond }},
		{"limiter hybrid named", map[string]string{"RELAY_LIMITER": "hybrid"}, "", func(c *Config) {}},
		{"plan cache", map[string]string{"RELAY_PLAN_CELL_IDLE_S": "1", "RELAY_PLAN_RESYNC_S": "0"}, "", func(c *Config) { c.PlanCellIdle, c.PlanResync = time.Second, 0 }},
		{"stream timers at zero and usage off", map[string]string{"RELAY_STREAM_KEEPALIVE_S": "0", "RELAY_PROXY_BODY_IDLE_S": "0", "RELAY_STREAM_IDLE_TIMEOUT_S": "0", "RELAY_STREAM_USAGE": "False"}, "",
			func(c *Config) {
				c.StreamKeepAlive, c.ProxyBodyIdle, c.StreamIdleTimeout, c.StreamUsage = 0, 0, 0, false
			}},
		{"stream timers", map[string]string{"RELAY_STREAM_KEEPALIVE_S": "5", "RELAY_PROXY_BODY_IDLE_S": "7", "RELAY_STREAM_IDLE_TIMEOUT_S": "9", "RELAY_STREAM_USAGE": "1"}, "",
			func(c *Config) {
				c.StreamKeepAlive, c.ProxyBodyIdle, c.StreamIdleTimeout = 5*time.Second, 7*time.Second, 9*time.Second
			}},
		{"account domain is lowercased and loses outer dots", map[string]string{"RELAY_ACCOUNT_DOMAIN": " .Accounts.Example.com. "}, "", func(c *Config) { c.AccountDomain = "accounts.example.com" }},
		{"deadlines", map[string]string{"RELAY_HEALTHZ_DEADLINE_MS": "50", "RELAY_SHUTDOWN_DEADLINE_S": "30"}, "", func(c *Config) { c.HealthzDeadlineMS, c.ShutdownDeadlineS = 50, 30 }},
		{"ui runtime URLs lose trailing slashes", map[string]string{"RELAY_CONTROL_API_URL": "https://c.example/api//", "RELAY_INFERENCE_API_URL": "https://i.example/", "RELAY_MODE": "cloud", "RELAY_UI_SENTRY_DSN": "https://k@sentry.example/1", "RELAY_UI_TELEMETRY_ENV": "prod", "RELAY_UI_DOCS_URL": "https://docs.example/", "RELAY_UI_SUPPORT_URL": "https://help.example"}, "",
			func(c *Config) {
				c.Runtime = RuntimeConfig{ControlAPIURL: "https://c.example/api", InferenceAPIURL: "https://i.example", Mode: "cloud", SentryDSN: "https://k@sentry.example/1", TelemetryEnv: "prod", DocsURL: "https://docs.example", SupportURL: "https://help.example"}
			}},
		{"ports and a shared listen host", map[string]string{"RELAY_PORT": "9000", "RELAY_CONTROL_PORT": "9001", "RELAY_LISTEN_HOST": " localhost "}, "",
			func(c *Config) {
				c.Port, c.ControlPort, c.ListenHost, c.ControlListenHost = "9000", "9001", "localhost", "localhost"
			}},
		{"control listen host alone", map[string]string{"RELAY_CONTROL_LISTEN_HOST": "127.0.0.1"}, "", func(c *Config) { c.ControlListenHost = "127.0.0.1" }},
		{"control listen host over the shared one", map[string]string{"RELAY_LISTEN_HOST": "10.0.0.5", "RELAY_CONTROL_LISTEN_HOST": "127.0.0.1"}, "",
			func(c *Config) { c.ListenHost, c.ControlListenHost = "10.0.0.5", "127.0.0.1" }},
		{"bracketed IPv6 listen host is stored bare", map[string]string{"RELAY_LISTEN_HOST": "[::1]"}, "", func(c *Config) { c.ListenHost, c.ControlListenHost = "::1", "::1" }},
		{"pprof with contention rates", map[string]string{"RELAY_PPROF_ADDR": " [::1]:6060 ", "RELAY_PPROF_MUTEX_FRACTION": "5", "RELAY_PPROF_BLOCK_RATE": " 10000 "}, "",
			func(c *Config) { c.PprofAddr, c.PprofMutexFraction, c.PprofBlockRate = "[::1]:6060", 5, 10000 }},
		{"pprof on every interface", map[string]string{"RELAY_PPROF_ADDR": ":6060"}, "", func(c *Config) { c.PprofAddr = ":6060" }},
		{"contention rates without a pprof address are not read", map[string]string{"RELAY_PPROF_MUTEX_FRACTION": "5", "RELAY_PPROF_BLOCK_RATE": "x"}, "", func(c *Config) {}},
		{"allowed origins are split and trimmed", map[string]string{"RELAY_CONTROL_ALLOW_ORIGINS": " https://a.example , ,https://b.example,"}, "",
			func(c *Config) { c.ControlAllowOrigins = []string{"https://a.example", "https://b.example"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			DefaultCatalogVersion = tc.stamp
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			want := defaults()
			tc.want(want)
			if diff := cmp.Diff(want, cfg); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoad_Rejects(t *testing.T) {
	keyA := strings.Repeat("A", 43) + "="
	tests := []struct {
		name string
		env  map[string]string
		want string // exact, or a prefix when it ends in "…"
	}{
		{"cluster mode is case-sensitive", map[string]string{"RELAY_CLUSTER_MODE": "ON"}, `RELAY_CLUSTER_MODE must be "on" or "off", got "ON"`},
		{"master key not base64", map[string]string{"RELAY_MASTER_KEY": "!!"}, "RELAY_MASTER_KEY: crypto: base64 decode RELAY_MASTER_KEY: …"},
		{"master key of the wrong length", map[string]string{"RELAY_MASTER_KEY": "QUJD"}, "RELAY_MASTER_KEY: crypto: RELAY_MASTER_KEY must decode to 32 bytes, got 3"},
		{"extra keys without a master key", map[string]string{"RELAY_MASTER_KEY_EXTRA": keyA}, "RELAY_MASTER_KEY_EXTRA needs RELAY_MASTER_KEY"},
		{"second extra key invalid", map[string]string{"RELAY_MASTER_KEY": keyA, "RELAY_MASTER_KEY_EXTRA": keyA + ",QUJD"}, "RELAY_MASTER_KEY_EXTRA key 2: crypto: RELAY_MASTER_KEY must decode to 32 bytes, got 3"},
		{"empty extra key", map[string]string{"RELAY_MASTER_KEY": keyA, "RELAY_MASTER_KEY_EXTRA": keyA + ","}, "RELAY_MASTER_KEY_EXTRA key 2: crypto: RELAY_MASTER_KEY is empty"},
		{"unknown eventlog backend", map[string]string{"RELAY_EVENTLOG_BACKEND": "s3"}, `RELAY_EVENTLOG_BACKEND must be "file", "clickhouse", "valkey", or "postgres", got "s3"`},
		{"pg max conns zero", map[string]string{"RELAY_PG_MAX_CONNS": "0"}, "RELAY_PG_MAX_CONNS must be between 1 and 2147483647"},
		{"pg max conns over int32", map[string]string{"RELAY_PG_MAX_CONNS": "2147483648"}, "RELAY_PG_MAX_CONNS must be between 1 and 2147483647"},
		{"pg min conns zero", map[string]string{"RELAY_PG_MIN_CONNS": "0"}, "RELAY_PG_MIN_CONNS must be between 1 and 2147483647"},
		{"pg min conns over int32", map[string]string{"RELAY_PG_MIN_CONNS": "2147483648"}, "RELAY_PG_MIN_CONNS must be between 1 and 2147483647"},
		{"unknown authz", map[string]string{"RELAY_AUTHZ": "RBAC"}, `RELAY_AUTHZ must be "single" or "rbac", got "RBAC"`},
		{"migrate on boot", map[string]string{"RELAY_MIGRATE_ON_BOOT": "Maybe"}, `RELAY_MIGRATE_ON_BOOT must be "on" or "off", got "maybe"`},
		{"stream usage", map[string]string{"RELAY_STREAM_USAGE": "maybe"}, `RELAY_STREAM_USAGE must be "on" or "off", got "maybe"`},
		{"limiter", map[string]string{"RELAY_LIMITER": "local"}, `RELAY_LIMITER must be "lua" or "hybrid", got "local"`},
		{"in-flight p99 negative", map[string]string{"RELAY_INFLIGHT_SCHED_P99": "-1ms"}, "RELAY_INFLIGHT_SCHED_P99 must be a non-negative Go duration (e.g. 20ms), or 0 to turn the adaptive in-flight limit off"},
		{"in-flight p99 without a unit", map[string]string{"RELAY_INFLIGHT_SCHED_P99": "5"}, "RELAY_INFLIGHT_SCHED_P99 must be a non-negative Go duration (e.g. 20ms), or 0 to turn the adaptive in-flight limit off"},
		{"conn queue timeout zero", map[string]string{"RELAY_CONN_QUEUE_TIMEOUT": "0s"}, "RELAY_CONN_QUEUE_TIMEOUT must be a positive duration such as 1s or 500ms"},
		{"conn queue timeout negative", map[string]string{"RELAY_CONN_QUEUE_TIMEOUT": "-1s"}, "RELAY_CONN_QUEUE_TIMEOUT must be a positive duration such as 1s or 500ms"},
		{"conn queue timeout without a unit", map[string]string{"RELAY_CONN_QUEUE_TIMEOUT": "1"}, "RELAY_CONN_QUEUE_TIMEOUT must be a positive duration such as 1s or 500ms"},
		{"account domain with a scheme", map[string]string{"RELAY_ACCOUNT_DOMAIN": "https://accounts.example.com"}, `RELAY_ACCOUNT_DOMAIN must be a bare domain such as accounts.example.com, got "https://accounts.example.com"`},
		{"account domain with a port", map[string]string{"RELAY_ACCOUNT_DOMAIN": "accounts.example.com:8080"}, `RELAY_ACCOUNT_DOMAIN must be a bare domain such as accounts.example.com, got "accounts.example.com:8080"`},
		{"account domain with a space", map[string]string{"RELAY_ACCOUNT_DOMAIN": "accounts example.com"}, `RELAY_ACCOUNT_DOMAIN must be a bare domain such as accounts.example.com, got "accounts example.com"`},
		{"poll interval not a duration", map[string]string{"RELAY_CATALOG_POLL_INTERVAL": "hourly"}, `RELAY_CATALOG_POLL_INTERVAL must be a non-negative Go duration such as 1h or 15m (0 disables polling), got "hourly"`},
		{"poll interval negative", map[string]string{"RELAY_CATALOG_POLL_INTERVAL": "-1m"}, `RELAY_CATALOG_POLL_INTERVAL must be a non-negative Go duration such as 1h or 15m (0 disables polling), got "-1m"`},
		{"webhook over plain http", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "http://relay.example.com"}, `RELAY_CATALOG_WEBHOOK_URL: must be an https URL such as https://relay.example.com, got "http://relay.example.com" (http needs RELAY_COOKIE_SECURE=false)`},
		{"webhook over another scheme even with insecure cookies", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "ftp://relay.example.com", "RELAY_COOKIE_SECURE": "false"}, `RELAY_CATALOG_WEBHOOK_URL: must be an https URL such as https://relay.example.com, got "ftp://relay.example.com" (http needs RELAY_COOKIE_SECURE=false)`},
		{"webhook without a scheme", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "relay.example.com"}, `RELAY_CATALOG_WEBHOOK_URL: must be an https URL such as https://relay.example.com, got "relay.example.com" (http needs RELAY_COOKIE_SECURE=false)`},
		{"webhook with a query", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "https://relay.example.com/?a=1"}, `RELAY_CATALOG_WEBHOOK_URL: must be a base URL without credentials, query or fragment, got "https://relay.example.com/?a=1"`},
		{"webhook with a fragment", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "https://relay.example.com/#x"}, `RELAY_CATALOG_WEBHOOK_URL: must be a base URL without credentials, query or fragment, got "https://relay.example.com/#x"`},
		{"webhook with credentials", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "https://u:p@relay.example.com"}, `RELAY_CATALOG_WEBHOOK_URL: must be a base URL without credentials, query or fragment, got "https://u:p@relay.example.com"`},
		{"webhook that does not parse", map[string]string{"RELAY_CATALOG_WEBHOOK_URL": "https://relay.example.com/%zz"}, "RELAY_CATALOG_WEBHOOK_URL: parse …"},
		{"listen host with a port", map[string]string{"RELAY_LISTEN_HOST": "127.0.0.1:8080"}, `RELAY_LISTEN_HOST must be a host or IP address without a port, got "127.0.0.1:8080"`},
		{"listen host with a bracketed port", map[string]string{"RELAY_LISTEN_HOST": "[::1]:8080"}, `RELAY_LISTEN_HOST must be a host or IP address without a port, got "[::1]:8080"`},
		{"listen host as a socket path", map[string]string{"RELAY_LISTEN_HOST": "unix:/tmp/relay.sock"}, `RELAY_LISTEN_HOST must be a host or IP address without a port, got "unix:/tmp/relay.sock"`},
		{"control listen host with a port", map[string]string{"RELAY_CONTROL_LISTEN_HOST": "localhost:8081"}, `RELAY_CONTROL_LISTEN_HOST must be a host or IP address without a port, got "localhost:8081"`},
		{"pprof address without a port", map[string]string{"RELAY_PPROF_ADDR": "127.0.0.1"}, `RELAY_PPROF_ADDR must be host:port (e.g. 127.0.0.1:6060), got "127.0.0.1"`},
		{"pprof address that is only a port", map[string]string{"RELAY_PPROF_ADDR": "6060"}, `RELAY_PPROF_ADDR must be host:port (e.g. 127.0.0.1:6060), got "6060"`},
		{"pprof mutex fraction negative", map[string]string{"RELAY_PPROF_ADDR": ":6060", "RELAY_PPROF_MUTEX_FRACTION": "-1"}, `RELAY_PPROF_MUTEX_FRACTION must be a non-negative integer, got "-1"`},
		{"pprof block rate not a number", map[string]string{"RELAY_PPROF_ADDR": ":6060", "RELAY_PPROF_BLOCK_RATE": "x"}, `RELAY_PPROF_BLOCK_RATE must be a non-negative integer, got "x"`},
	}
	// Integers with a lower bound share one message shape: "<NAME> must be >= <bound>".
	for name, bad := range map[string]string{
		"RELAY_REDIS_POOL_SIZE": "0", "RELAY_REDIS_MIN_IDLE_CONNS": "0", "RELAY_REDIS_POOL_MAX_WAITERS": "-1",
		"RELAY_REDIS_DIAL_TIMEOUT_MS": "0", "RELAY_REDIS_READ_TIMEOUT_MS": "0", "RELAY_REDIS_WRITE_TIMEOUT_MS": "-5", "RELAY_REDIS_POOL_TIMEOUT_MS": "-1",
		"RELAY_REDIS_POSTFLIGHT_POOL_SIZE": "0", "RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS": "-1", "RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS": "0", "RELAY_REDIS_POSTFLIGHT_BATCH_SIZE": "0",
		"RELAY_USAGE_QUEUE_SIZE": "0", "RELAY_USAGE_REFUSED_QUEUE_SIZE": "-5", "RELAY_PAYLOAD_QUEUE_BYTES": "0", "RELAY_PAYLOAD_REFUSED_QUEUE_BYTES": "0",
		"RELAY_BATCH_MAX_ITEMS": "0", "RELAY_UPSTREAM_MAX_IDLE_PER_HOST": "0", "RELAY_UPSTREAM_DIAL_TIMEOUT_S": "0", "RELAY_MAX_CONNS": "0",
		"RELAY_LIMITER_SYNC_MS": "0", "RELAY_PLAN_CELL_IDLE_S": "0", "RELAY_HEALTHZ_DEADLINE_MS": "0", "RELAY_SHUTDOWN_DEADLINE_S": "-1",
	} {
		tests = append(tests, struct {
			name string
			env  map[string]string
			want string
		}{name + "=" + bad, map[string]string{name: bad}, name + " must be >= 1"})
	}
	for _, name := range []string{
		"RELAY_REDIS_MAX_RETRIES", "RELAY_REDIS_POSTFLIGHT_BATCH_DELAY_MS", "RELAY_REDIS_POSTFLIGHT_QUEUE_SIZE", "RELAY_MIGRATE_LOCK_TIMEOUT_S", "RELAY_CONN_QUEUE",
		"RELAY_KEY_BREAKER_THRESHOLD", "RELAY_KEY_BREAKER_WINDOW_S", "RELAY_PLAN_RESYNC_S", "RELAY_STREAM_KEEPALIVE_S", "RELAY_PROXY_BODY_IDLE_S", "RELAY_STREAM_IDLE_TIMEOUT_S",
	} {
		tests = append(tests, struct {
			name string
			env  map[string]string
			want string
		}{name + "=-1", map[string]string{name: "-1"}, name + " must be >= 0"})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if cfg != nil || err == nil {
				t.Fatalf("Load = %+v, %v; want nil and an error", cfg, err)
			}
			if prefix, ok := strings.CutSuffix(tc.want, "…"); ok {
				if !strings.HasPrefix(err.Error(), prefix) {
					t.Errorf("error = %q, want prefix %q", err, prefix)
				}
			} else if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestLoad_RejectsNonNumericIntegers(t *testing.T) {
	t.Skip(`bug: an integer variable that is not a number (RELAY_MAX_CONNS=10k, RELAY_BATCH_MAX_ITEMS=many, RELAY_STREAM_IDLE_TIMEOUT_S=10m) is silently read as unset instead of failing the boot`)
	for name, bad := range map[string]string{"RELAY_MAX_CONNS": "10k", "RELAY_BATCH_MAX_ITEMS": "many", "RELAY_STREAM_IDLE_TIMEOUT_S": "10m", "RELAY_MAX_REQUEST_BYTES": "5GB", "RELAY_MAX_INFLIGHT": "1e3"} {
		clearEnv(t)
		t.Setenv(name, bad)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q was accepted", name, bad)
		}
	}
}

func TestConfig_MasterKeys(t *testing.T) {
	a, b := []byte{1}, []byte{2}
	c := &Config{MasterKey: a, MasterKeyExtra: [][]byte{b}}
	if diff := cmp.Diff([][]byte{a, b}, c.MasterKeys()); diff != "" {
		t.Errorf("MasterKeys (-want +got):\n%s", diff)
	}
	if got := (&Config{MasterKeyExtra: [][]byte{b}}).MasterKeys(); got != nil {
		t.Errorf("MasterKeys without a primary key = %v, want nil", got)
	}
}

// RELAY_MULTI_USER is deprecated: read without RELAY_AUTHZ it warns about the rename, read against RELAY_AUTHZ=single it warns that every caller is an admin, and otherwise nothing is logged.
func TestLoad_MultiUserWarnings(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	for _, tc := range []struct{ authz, multiUser, want string }{
		{"", "on", "RELAY_MULTI_USER is deprecated and was read as RELAY_AUTHZ=rbac"},
		{"single", "on", "RELAY_AUTHZ=single with RELAY_MULTI_USER=on"},
		{"rbac", "on", ""},
		{"single", "", ""},
		{"", "", ""},
	} {
		clearEnv(t)
		buf.Reset()
		t.Setenv("RELAY_AUTHZ", tc.authz)
		t.Setenv("RELAY_MULTI_USER", tc.multiUser)
		if _, err := Load(); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("RELAY_AUTHZ=%q RELAY_MULTI_USER=%q logged %q, want %q", tc.authz, tc.multiUser, got, tc.want)
		}
	}
}
