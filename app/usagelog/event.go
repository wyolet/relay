package usagelog

import "github.com/wyolet/relay/pkg/usage"

// Event is the canonical per-request usage record. The canonical type
// lives in pkg/usage so every backend (file, ClickHouse, valkey,
// postgres) can consume it without importing app/ (pkg-purity rule).
// This alias keeps the lifecycle observers in this package ergonomic.
type Event = usage.Event

// UpstreamTiming is the upstream-leg timing breakdown — see
// pkg/usage.UpstreamTiming.
type UpstreamTiming = usage.UpstreamTiming

// ReasoningTiming is the reasoning span — see pkg/usage.ReasoningTiming.
type ReasoningTiming = usage.ReasoningTiming
