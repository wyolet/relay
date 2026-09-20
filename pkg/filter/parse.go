package filter

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Query is a parsed, validated request ready to Apply to a slice of T.
type Query[T any] struct {
	schema    Schema[T]
	preds     []func(*T) bool
	q         string            // lower-cased free-text needle; "" = no text filter
	labels    map[string]string // label selectors (k=v); all must match (AND)
	sortField *Field[T]
	sortDesc  bool
	limit     int // 0 = no limit
	offset    int
}

// reserved (non-field) query params the engine owns.
var reserved = map[string]bool{"q": true, "sort": true, "limit": true, "offset": true, "label": true}

// Parse validates raw against the schema and compiles it into a Query.
// Unknown keys, malformed values, out-of-enum values, and non-sortable
// sort targets all return an *Error.
func (s Schema[T]) Parse(raw url.Values) (Query[T], error) {
	byName := make(map[string]*Field[T], len(s.Fields))
	for i := range s.Fields {
		byName[s.Fields[i].Name] = &s.Fields[i]
	}

	q := Query[T]{schema: s}

	// Validate every supplied key against the allowlist before doing work,
	// so a typo never silently widens the result set.
	for key, vals := range raw {
		if reserved[key] {
			continue
		}
		base, suffix := splitSuffix(key)
		f := byName[base]
		if f == nil {
			return Query[T]{}, &Error{Key: key, Msg: "unknown filter field"}
		}
		pred, err := f.compile(key, suffix, vals)
		if err != nil {
			return Query[T]{}, err
		}
		if pred != nil {
			q.preds = append(q.preds, pred)
		}
	}

	if qs := raw.Get("q"); qs != "" {
		if s.Q == nil {
			return Query[T]{}, &Error{Key: "q", Msg: "free-text search not supported for this resource"}
		}
		q.q = strings.ToLower(qs)
	}

	for _, sel := range raw["label"] {
		if sel == "" {
			continue
		}
		if s.Labels == nil {
			return Query[T]{}, &Error{Key: "label", Msg: "label selectors not supported for this resource"}
		}
		k, v, ok := strings.Cut(sel, "=")
		if !ok || k == "" {
			return Query[T]{}, &Error{Key: "label", Msg: "must be key=value"}
		}
		if q.labels == nil {
			q.labels = map[string]string{}
		}
		q.labels[k] = v
	}

	sortSpec := raw.Get("sort")
	if sortSpec == "" {
		sortSpec = s.DefaultSort
	}
	if sortSpec != "" {
		name := strings.TrimPrefix(sortSpec, "-")
		f := byName[name]
		if f == nil || !f.Sortable {
			return Query[T]{}, &Error{Key: "sort", Msg: fmt.Sprintf("cannot sort by %q", name)}
		}
		q.sortField = f
		q.sortDesc = strings.HasPrefix(sortSpec, "-")
	}

	if v := raw.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Query[T]{}, &Error{Key: "limit", Msg: "must be a non-negative integer"}
		}
		if n > MaxLimit {
			n = MaxLimit
		}
		q.limit = n // explicit 0 = full set (opt out of DefaultLimit)
	} else {
		q.limit = s.DefaultLimit
	}
	if v := raw.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Query[T]{}, &Error{Key: "offset", Msg: "must be a non-negative integer"}
		}
		q.offset = n
	}

	return q, nil
}

// splitSuffix peels a range suffix (_min/_max/_from/_to) off a key,
// returning the base field name and the suffix ("" if none).
func splitSuffix(key string) (base, suffix string) {
	for _, sfx := range []string{"_min", "_max", "_from", "_to"} {
		if strings.HasSuffix(key, sfx) {
			return strings.TrimSuffix(key, sfx), sfx
		}
	}
	return key, ""
}
