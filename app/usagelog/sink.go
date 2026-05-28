package usagelog

import "github.com/wyolet/relay/sdk/usage"

// Sink consumes usage events. Canonical interface in pkg/usage; the
// concrete backends (file, clickhouse, valkey, postgres) live under
// pkg/usage/<backend> and implement it. Re-exported here for the Emitter
// and the composition root.
type Sink = usage.Sink

// Closer is the optional graceful-shutdown contract a Sink may implement;
// the Emitter calls it after draining. See pkg/usage.Closer.
type Closer = usage.Closer
