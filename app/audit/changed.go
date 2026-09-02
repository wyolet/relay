package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// secretSegments are path leaves whose mere name marks the path as
// secret-bearing. Dropping the path (not just the value) keeps an audit row
// from becoming a map of where the credentials live.
var secretSegments = map[string]bool{
	"keyHash": true, "previousKeyHash": true, "value": true,
	"password": true, "passwordHash": true, "token": true, "secret": true,
}

// serverOwnedPaths are stamped by the server, not sent by the caller, so
// they differ on every update and say nothing about what an operator did.
var serverOwnedPaths = map[string]bool{
	"metadata.createdAt": true,
	"metadata.updatedAt": true,
	"metadata.dirty":     true,
}

// AnyField is the field list for a whole-row write (create, delete), where
// naming individual paths says nothing the action doesn't.
const AnyField = "*"

// Changed attaches the JSON paths a write touched to the in-flight event.
// Secret-bearing paths are dropped here, so no call site has to remember
// to. No-op outside an audited request.
func Changed(ctx context.Context, fields []string) {
	f := fromContext(ctx)
	if f == nil {
		return
	}
	kept := make([]string, 0, len(fields))
	for _, p := range fields {
		if p == "" || secretSegments[leafOf(p)] {
			continue
		}
		kept = append(kept, p)
	}
	f.mu.Lock()
	if len(kept) == 0 {
		f.change = &Change{}
	} else {
		f.change = &Change{Fields: kept}
	}
	f.mu.Unlock()
}

// DiffFields lists the JSON paths that differ between two entity values,
// one level under each top-level object ("metadata.displayName",
// "spec.enabled"). One level is enough to say what an operator edited
// without reconstructing the value.
func DiffFields(existing, incoming any) []string {
	a, b := objectOf(existing), objectOf(incoming)
	seen := map[string]bool{}
	var out []string
	for _, top := range union(a, b) {
		sub, subOK := nestedObject(a[top]), nestedObject(b[top])
		if sub == nil && subOK == nil {
			if !bytes.Equal(a[top], b[top]) && !seen[top] {
				seen[top] = true
				out = append(out, top)
			}
			continue
		}
		for _, k := range union(sub, subOK) {
			if bytes.Equal(sub[k], subOK[k]) {
				continue
			}
			p := top + "." + k
			if serverOwnedPaths[p] {
				continue
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Record writes an event for a handler that never calls Authorize (login,
// logout). actorOverride replaces the middleware's actor — a failed login
// has no session to identify the caller, only the attempted username.
func Record(ctx context.Context, action string, res Resource, status string, actorOverride ...Actor) {
	f := fromContext(ctx)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.forced = &decision{Action: action, Resource: res, Status: status}
	if len(actorOverride) > 0 {
		ip := f.actor.IP
		f.actor = actorOverride[0]
		if f.actor.IP == "" {
			f.actor.IP = ip
		}
	}
	f.mu.Unlock()
}

func leafOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// objectOf marshals v and re-reads it as a shallow map of raw values, so
// comparison is over encoded JSON rather than Go types.
func objectOf(v any) map[string]json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func nestedObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func union(a, b map[string]json.RawMessage) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]json.RawMessage{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
