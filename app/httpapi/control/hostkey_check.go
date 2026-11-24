// Asking a host which provider organization a key value belongs to: the one read POST /host-keys/by-id/{id}/detect-account makes on a stored key, also made on a value not stored yet (POST /pseudo-host-keys).

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/adapter"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/catalogkind/host"
)

// keyCheckOutcome is what a host did when asked about a key value.
type keyCheckOutcome string

const (
	keyDetected    keyCheckOutcome = "detected"
	keyRefused     keyCheckOutcome = "refused"
	keyUnsupported keyCheckOutcome = "unsupported"
	keyUnreachable keyCheckOutcome = "unreachable"
)

// keyCheck is a host's answer to which organization a key value belongs to, told apart by what the host did: detected (Org names it), refused (it answered 401 or 403 to the key), unsupported (it names no organization: no shape of its bindings asks it, or it answered without one), unreachable (no answer, or another status). Status is the status it answered with, 0 for none; Reason says what happened for every outcome but detected, naming the host and never the value.
type keyCheck struct {
	Outcome keyCheckOutcome
	Org     adapter.Org
	Status  int
	Reason  string
}

// checkKeyValue asks host hostID, as the catalog has it, which organization value belongs to.
func checkKeyValue(ctx context.Context, snap *appcatalog.Snapshot, specs *adapter.Registry, hostID, value string) keyCheck {
	h, spec, reason := probeTarget(snap, specs, hostID)
	if spec == nil {
		return keyCheck{Outcome: keyUnsupported, Reason: reason}
	}
	return probeValue(ctx, spec, h, value, false)
}

// probeTarget is host hostID and the wire shape to probe it with: the first of the host's bindings, by name, whose shape names organizations. Without one, the shape is nil and reason says why.
func probeTarget(snap *appcatalog.Snapshot, specs *adapter.Registry, hostID string) (*host.Host, *adapter.Spec, string) {
	h, ok := snap.Host(hostID)
	if !ok || !h.Routable() {
		return nil, nil, "the key's host is not loaded or has no base URL"
	}
	for _, b := range snap.AllBindings() {
		if b.Spec.HostID != h.Meta.ID {
			continue
		}
		if s := specs.Spec(b.Spec.Adapter); s != nil && s.Org != nil {
			return h, s, ""
		}
	}
	return nil, nil, fmt.Sprintf("host %q does not name the organization a key belongs to", h.Meta.Name)
}

// probeValue asks h through spec which organization value belongs to: one read of h's base URL as the catalog configures it, with the shape's sync timeout.
func probeValue(ctx context.Context, spec *adapter.Spec, h *host.Host, value string, oauth bool) keyCheck {
	org, err := spec.ProbeOrg(ctx, h.Spec.BaseURL, value, oauth)
	var status *adapter.ProbeStatusError
	switch {
	case err == nil:
		return keyCheck{Outcome: keyDetected, Org: org}
	case errors.Is(err, adapter.ErrNoOrgHeader):
		return keyCheck{Outcome: keyUnsupported, Reason: fmt.Sprintf("host %q answered without naming an organization", h.Meta.Name)}
	case errors.As(err, &status):
		c := keyCheck{Outcome: keyUnreachable, Status: status.Status, Reason: fmt.Sprintf("host %q answered %d", h.Meta.Name, status.Status)}
		if status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
			c.Outcome = keyRefused
		}
		return c
	default:
		return keyCheck{Outcome: keyUnreachable, Reason: fmt.Sprintf("host %q could not be reached: %v", h.Meta.Name, err)}
	}
}

// detectError is detect-account's answer to a check that named no organization: 422 when the host names none, 502 when it answered another status or could not be reached.
func (c keyCheck) detectError() error {
	switch {
	case c.Outcome == keyUnsupported:
		return huma.Error422UnprocessableEntity(c.Reason)
	case c.Status != 0:
		return huma.Error502BadGateway(c.Reason, &huma.ErrorDetail{Message: "upstream status", Location: "upstream.status", Value: c.Status})
	default:
		return huma.Error502BadGateway(c.Reason)
	}
}
