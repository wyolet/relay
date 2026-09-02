package audit

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// mutatingVerbs are the action verbs that earn a row whatever the outcome.
var mutatingVerbs = map[string]bool{
	"create": true, "update": true, "delete": true, "rotate": true,
	"attach": true, "detach": true, "generate": true, "reload": true,
	"apply": true, "mint": true,
}

// decision is one Authorize call's contribution to the in-flight event.
type decision struct {
	Action   string
	Resource Resource
	Status   string
}

// inflight accumulates a request's decisions until the middleware emits.
type inflight struct {
	mu        sync.Mutex
	actor     Actor
	request   Request
	readRoute bool
	decisions []decision
	change    *Change
	// forced is set by Record for handlers that never call Authorize; it
	// bypasses the marking rule entirely.
	forced *decision
}

type ctxKey struct{}

func withInflight(ctx context.Context, f *inflight) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

func fromContext(ctx context.Context) *inflight {
	f, _ := ctx.Value(ctxKey{}).(*inflight)
	return f
}

func (f *inflight) add(d decision) {
	f.mu.Lock()
	f.decisions = append(f.decisions, d)
	f.mu.Unlock()
}

// event assembles the row to write, or reports false when the request
// earns none.
func (f *inflight) event(code int) (Event, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.forced, f.forced != nil
	if !ok {
		var found decision
		found, ok = markDecision(f.decisions, f.readRoute)
		d = &found
	}
	if !ok && len(f.decisions) == 0 {
		// A mutation the handler refused before authorizing — a row hidden
		// by visibility scoping 404s without an Authorize call, and would
		// otherwise leave no trace of the attempt.
		var found decision
		found, ok = refusedRoute(f.request.Method, f.request.Path, code)
		d = &found
	}
	if !ok {
		return Event{}, false
	}
	return Event{
		Actor:    f.actor,
		Action:   d.Action,
		Resource: d.Resource,
		Outcome:  Outcome{Status: d.Status, Code: code},
		Request:  f.request,
		Change:   f.change,
	}, true
}

// markDecision applies the marking rule to a request's Authorize calls.
//
// A mutating verb always wins, and the last one does: a handler may probe
// broader permissions before deciding (usage checks read_all, then read),
// and only the decision the handler acted on describes the request. On a
// read route a denied probe is therefore invisible — only the final call's
// denial marks. Off a read route any denial marks, because a non-read
// request that was refused anywhere did not do what it set out to do.
func markDecision(ds []decision, readRoute bool) (decision, bool) {
	for i := len(ds) - 1; i >= 0; i-- {
		if mutatingVerbs[verbOf(ds[i].Action)] {
			return ds[i], true
		}
	}
	if len(ds) == 0 {
		return decision{}, false
	}
	if readRoute {
		if last := ds[len(ds)-1]; last.Status != StatusAllowed {
			return last, true
		}
		return decision{}, false
	}
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Status != StatusAllowed {
			return ds[i], true
		}
	}
	return decision{}, false
}

// refusedRoute reconstructs the attempted action from the route when a
// mutation was refused before any Authorize call ran. Only 401/403/404 on a
// non-read method qualifies: any other status means the handler got far
// enough to authorize, or the request was never a mutation.
func refusedRoute(method, path string, code int) (decision, bool) {
	if isReadRoute(method) {
		return decision{}, false
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
	default:
		return decision{}, false
	}
	segs := pathSegments(path)
	if len(segs) == 0 {
		return decision{}, false
	}
	plural := segs[0]
	verb, id := "", ""
	for i := 1; i < len(segs); i++ {
		switch segs[i] {
		case "by-id":
			if i+1 < len(segs) {
				id = segs[i+1]
			}
		case "rotate", "attach", "detach":
			verb = segs[i]
		}
	}
	if verb == "" {
		switch method {
		case http.MethodPost:
			verb = "create"
		case http.MethodPut, http.MethodPatch:
			verb = "update"
		case http.MethodDelete:
			verb = "delete"
		default:
			return decision{}, false
		}
	}
	return decision{
		Action:   plural + "." + verb,
		Resource: Resource{Kind: plural, ID: id},
		Status:   StatusDenied,
	}, true
}

// pathSegments splits a control-plane path into its segments, dropping the
// "/api" mount prefix the control router may or may not carry.
func pathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	if len(out) > 0 && out[0] == "api" {
		out = out[1:]
	}
	return out
}

// verbOf is the last dot-separated segment of an action ("models.overlay
// .update" → "update").
func verbOf(action string) string {
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		return action[i+1:]
	}
	return action
}

// isReadRoute reports whether the HTTP method is a pure read.
func isReadRoute(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}
