package usagelog

import "github.com/wyolet/relay/pkg/usage"

// Sink consumes usage events. Canonical interface in pkg/usage; the
// concrete backends (file, clickhouse, valkey, postgres) live under
// pkg/usage/<backend> and implement it. Re-exported here for the Emitter
// and the composition root.
type Sink = usage.Sink

// BatchWriter is the optional bulk-write path a Sink may implement; the
// Emitter uses it for each drained batch. See pkg/usage.BatchWriter.
type BatchWriter = usage.BatchWriter

// Closer is the optional graceful-shutdown contract a Sink may implement;
// the Emitter calls it after draining. See pkg/usage.Closer.
type Closer = usage.Closer
