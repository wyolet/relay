package usage

import "strings"

// ValidGroupBy lists the accepted static GroupBy values. Used by the HTTP
// endpoint to reject typos with a clear 400 instead of silently
// grouping on nothing. "tags.<key>" and "extras.<key>" are additionally
// accepted as dynamic dimensions — see MapGroupKey.
var ValidGroupBy = []string{
	"relay_key_hash",
	"policy_id",
	"model_id",
	"host_id",
	"host_key_id",
	"source",
	"finish_reason",
	"error_kind",
	"model",
	"host",
	"policy",
	"provider",
	"project_id",
	"team_id",
	"principal_id",
	"credential_id",
	"project",
	"team",
	"principal",
	"relay_key",
}

// MaxTagKeyLen caps a single tag key. Enforced at write time
// (app/usagelog tag validation) and reused by MapGroupKey so an
// unwritable key can't become a group dimension.
const MaxTagKeyLen = 64

// MapGroupKey splits a dynamic map group dimension — "tags.<key>" (caller
// tags) or "extras.<key>" (relay-stamped provenance: instance, client_ip,
// resolved_via) — into the Event map column and the key within it. ok is
// false when g isn't map-shaped or the key is empty / over MaxTagKeyLen.
// Backends MUST bind the returned key as a query parameter, never splice
// it into SQL text; column is one of the two fixed names and safe to embed.
func MapGroupKey(g string) (column, key string, ok bool) {
	for _, column = range []string{"tags", "extras"} {
		key, found := strings.CutPrefix(g, column+".")
		if !found {
			continue
		}
		if key == "" || len(key) > MaxTagKeyLen {
			return "", "", false
		}
		return column, key, true
	}
	return "", "", false
}

// IsValidGroupBy reports whether g is one of the supported group
// dimensions (a ValidGroupBy entry or a dynamic map one).
func IsValidGroupBy(g string) bool {
	for _, v := range ValidGroupBy {
		if g == v {
			return true
		}
	}
	_, _, ok := MapGroupKey(g)
	return ok
}

func groupKey(ev Event, groupBy string) string {
	if column, key, ok := MapGroupKey(groupBy); ok {
		if column == "extras" {
			return ev.Extras[key]
		}
		return ev.Tags[key]
	}
	switch groupBy {
	case "relay_key_hash":
		return ev.RelayKeyHash
	case "policy_id":
		return ev.PolicyID
	case "project_id":
		return ev.ProjectID
	case "team_id":
		return ev.TeamID
	case "principal_id":
		return ev.PrincipalID
	case "credential_id":
		return ev.CredentialID
	case "model_id":
		return ev.ModelID
	case "host_id":
		return ev.HostID
	case "host_key_id":
		return ev.HostKeyID
	case "finish_reason":
		return ev.FinishReason
	case "error_kind":
		return ev.ErrorKind
	case "model":
		return ev.Model
	case "host":
		return ev.Host
	case "policy":
		return ev.Policy
	case "provider":
		return ev.Provider
	case "project":
		return ev.Project
	case "team":
		return ev.Team
	case "principal":
		return ev.Principal
	case "relay_key":
		return ev.RelayKey
	default: // "source"
		return ev.Source
	}
}
