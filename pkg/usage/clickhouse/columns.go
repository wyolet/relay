package clickhouse

import (
	"time"

	"github.com/ClickHouse/ch-go/proto"
	"github.com/wyolet/relay/pkg/usage"
)

// eventColumns holds one typed buffer per insertColumns entry. A sink keeps
// one set and resets it after every batch, so the buffers keep their
// capacity across batches and no row is boxed or walked by reflection.
type eventColumns struct {
	requestID             proto.ColStr
	source                *proto.ColLowCardinality[string]
	ts                    *proto.ColDateTime64
	status                proto.ColUInt16
	durationMs            proto.ColInt64
	streamed              proto.ColUInt8
	finishReason          *proto.ColLowCardinality[string]
	attempts              proto.ColUInt16
	errorKind             *proto.ColLowCardinality[string]
	errorMessage          proto.ColStr
	upstreamStart         proto.ColInt64
	upstreamResponseStart proto.ColInt64
	upstreamResponseEnd   proto.ColInt64
	relayKeyHash          proto.ColStr
	policyID              proto.ColStr
	modelID               proto.ColStr
	requestedModel        *proto.ColLowCardinality[string]
	hostID                proto.ColStr
	hostKeyID             proto.ColStr
	tokens                *proto.ColMap[string, int64]
	extras                *proto.ColMap[string, string]
	tags                  *proto.ColMap[string, string]
	model                 *proto.ColLowCardinality[string]
	host                  *proto.ColLowCardinality[string]
	policy                *proto.ColLowCardinality[string]
	provider              *proto.ColLowCardinality[string]
	costNanos             proto.ColInt64
	costBreakdown         *proto.ColMap[string, int64]
	pricing               *proto.ColLowCardinality[string]
	projectID             proto.ColStr
	project               *proto.ColLowCardinality[string]
	teamID                proto.ColStr
	team                  *proto.ColLowCardinality[string]
	principalKind         *proto.ColLowCardinality[string]
	principalID           proto.ColStr
	principal             *proto.ColLowCardinality[string]
	credentialKind        *proto.ColLowCardinality[string]
	credentialID          proto.ColStr
	relayKey              *proto.ColLowCardinality[string]

	input proto.Input
	// lowCardinality lists every LowCardinality buffer, map keys included:
	// they hold the events' strings rather than copies, so reset clears them.
	lowCardinality []*proto.ColLowCardinality[string]
}

func newEventColumns() *eventColumns {
	c := &eventColumns{}
	lc := func() *proto.ColLowCardinality[string] {
		col := new(proto.ColStr).LowCardinality()
		c.lowCardinality = append(c.lowCardinality, col)
		return col
	}
	c.source = lc()
	c.ts = new(proto.ColDateTime64).WithPrecision(proto.PrecisionNano).WithLocation(time.UTC)
	c.finishReason = lc()
	c.errorKind = lc()
	c.requestedModel = lc()
	c.tokens = proto.NewMap[string, int64](lc(), new(proto.ColInt64))
	c.extras = proto.NewMap[string, string](lc(), new(proto.ColStr))
	c.tags = proto.NewMap[string, string](lc(), new(proto.ColStr))
	c.model = lc()
	c.host = lc()
	c.policy = lc()
	c.provider = lc()
	c.costBreakdown = proto.NewMap[string, int64](lc(), new(proto.ColInt64))
	c.pricing = lc()
	c.project = lc()
	c.team = lc()
	c.principalKind = lc()
	c.principal = lc()
	c.credentialKind = lc()
	c.relayKey = lc()

	c.input = proto.Input{
		{Name: "request_id", Data: &c.requestID},
		{Name: "source", Data: c.source},
		{Name: "ts", Data: c.ts},
		{Name: "status", Data: &c.status},
		{Name: "duration_ms", Data: &c.durationMs},
		{Name: "streamed", Data: &c.streamed},
		{Name: "finish_reason", Data: c.finishReason},
		{Name: "attempts", Data: &c.attempts},
		{Name: "error_kind", Data: c.errorKind},
		{Name: "error_message", Data: &c.errorMessage},
		{Name: "upstream_start", Data: &c.upstreamStart},
		{Name: "upstream_response_start", Data: &c.upstreamResponseStart},
		{Name: "upstream_response_end", Data: &c.upstreamResponseEnd},
		{Name: "relay_key_hash", Data: &c.relayKeyHash},
		{Name: "policy_id", Data: &c.policyID},
		{Name: "model_id", Data: &c.modelID},
		{Name: "requested_model", Data: c.requestedModel},
		{Name: "host_id", Data: &c.hostID},
		{Name: "host_key_id", Data: &c.hostKeyID},
		{Name: "tokens", Data: c.tokens},
		{Name: "extras", Data: c.extras},
		{Name: "tags", Data: c.tags},
		{Name: "model", Data: c.model},
		{Name: "host", Data: c.host},
		{Name: "policy", Data: c.policy},
		{Name: "provider", Data: c.provider},
		{Name: "cost_nanos", Data: &c.costNanos},
		{Name: "cost_breakdown", Data: c.costBreakdown},
		{Name: "pricing", Data: c.pricing},
		{Name: "project_id", Data: &c.projectID},
		{Name: "project", Data: c.project},
		{Name: "team_id", Data: &c.teamID},
		{Name: "team", Data: c.team},
		{Name: "principal_kind", Data: c.principalKind},
		{Name: "principal_id", Data: &c.principalID},
		{Name: "principal", Data: c.principal},
		{Name: "credential_kind", Data: c.credentialKind},
		{Name: "credential_id", Data: &c.credentialID},
		{Name: "relay_key", Data: c.relayKey},
	}
	return c
}

// appendEvents appends one row per event, with the values insertValues
// gives the same event.
func (c *eventColumns) appendEvents(events []usage.Event) {
	for i := range events {
		c.appendEvent(&events[i])
	}
}

func (c *eventColumns) appendEvent(ev *usage.Event) {
	// Sentinel -1 means "no upstream timing available" in Int64 columns.
	var upStart, upRespStart, upRespEnd int64 = -1, -1, -1
	if ev.Upstream != nil {
		upStart = ev.Upstream.Start
		upRespStart = ev.Upstream.ResponseStart
		upRespEnd = ev.Upstream.ResponseEnd
	}
	streamed := uint8(0)
	if ev.Streamed {
		streamed = 1
	}
	// Sentinel -1 = unpriced (cost is never negative; a priced $0 stays 0).
	costNanos := int64(-1)
	if ev.CostNanos != nil {
		costNanos = *ev.CostNanos
	}

	c.requestID.Append(ev.RequestID)
	c.source.Append(ev.Source)
	c.ts.Append(ev.Timestamp)
	c.status.Append(uint16(ev.Status))
	c.durationMs.Append(ev.DurationMs)
	c.streamed.Append(streamed)
	c.finishReason.Append(ev.FinishReason)
	c.attempts.Append(uint16(ev.Attempts))
	c.errorKind.Append(ev.ErrorKind)
	c.errorMessage.Append(ev.ErrorMessage)
	c.upstreamStart.Append(upStart)
	c.upstreamResponseStart.Append(upRespStart)
	c.upstreamResponseEnd.Append(upRespEnd)
	c.relayKeyHash.Append(ev.RelayKeyHash)
	c.policyID.Append(ev.PolicyID)
	c.modelID.Append(ev.ModelID)
	c.requestedModel.Append(ev.RequestedModel)
	c.hostID.Append(ev.HostID)
	c.hostKeyID.Append(ev.HostKeyID)
	c.tokens.Append(ev.Tokens)
	c.extras.Append(ev.Extras)
	c.tags.Append(ev.Tags)
	c.model.Append(ev.Model)
	c.host.Append(ev.Host)
	c.policy.Append(ev.Policy)
	c.provider.Append(ev.Provider)
	c.costNanos.Append(costNanos)
	c.costBreakdown.Append(ev.CostBreakdown)
	c.pricing.Append(ev.Pricing)
	c.projectID.Append(ev.ProjectID)
	c.project.Append(ev.Project)
	c.teamID.Append(ev.TeamID)
	c.team.Append(ev.Team)
	c.principalKind.Append(ev.PrincipalKind)
	c.principalID.Append(ev.PrincipalID)
	c.principal.Append(ev.Principal)
	c.credentialKind.Append(ev.CredentialKind)
	c.credentialID.Append(ev.CredentialID)
	c.relayKey.Append(ev.RelayKey)
}

// reset empties every column and keeps its capacity.
func (c *eventColumns) reset() {
	for _, col := range c.lowCardinality {
		clear(col.Values)
	}
	c.input.Reset()
}
