// Package filter is a declarative, allowlist-based query engine for list
// endpoints. A resource declares a Schema[T] of typed Field accessors; the
// engine parses url.Values into a validated Query and applies it to an
// in-memory []*T (filter -> sort -> window), returning the page plus the
// pre-window total.
//
// The contract (shared with the relay UI's filter convention):
//
//   - Equality:        ?field=value
//   - One-of (IN):     repeat the key — ?id=a&id=b  (OR within a field)
//   - Boolean:         ?field=true|false
//   - Numeric range:   ?field_min=  / ?field_max=
//   - Time range:      ?field_from= / ?field_to=    (RFC3339)
//   - Free-text:       ?q=...        (Schema-chosen corpus, case-insensitive)
//   - Sort:            ?sort=field   ('-' prefix = descending)
//   - Window:          ?limit= / ?offset=
//
// Filters compose with AND; repeated same-key values are OR within that
// field. Any query key not in the schema's allowlist (or a malformed value)
// is rejected with an *Error — the HTTP layer maps it to 400 so typos
// surface instead of silently matching everything.
//
// Out of scope: SQL pushdown. This engine filters a materialised slice; it
// suits the config catalog (hundreds of rows read from the in-memory
// snapshot/store). The usage/event path has its own store-aware query type
// (pkg/usage.EventQuery) because it pushes filters into ClickHouse SQL.
package filter

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind tags a field's value domain, which selects the parse + match rules
// and the accepted query-param spelling (range fields gain _min/_max or
// _from/_to suffixes).
type Kind int

const (
	// String matches by exact equality. With Repeat, the key may appear
	// multiple times and matches if the field equals ANY given value (IN).
	// Use Get for a single-valued field or GetMulti for a slice field
	// (matches if any element equals any requested value).
	String Kind = iota
	// Bool matches ?field=true|false against a bool accessor.
	Bool
	// Int matches an inclusive numeric range via ?field_min / ?field_max.
	Int
	// Time matches an inclusive instant range via ?field_from / ?field_to,
	// each an RFC3339 timestamp.
	Time
)

// Field declares one filterable/sortable dimension of T. Exactly one
// accessor must be set, matching Kind (String -> Get or GetMulti; Bool ->
// GetBool; Int -> GetInt; Time -> GetTime). The accessor is a typed
// closure over T, so renaming the underlying struct field is a compile
// error here — the param name, allowlist entry, and match logic all derive
// from this single declaration.
type Field[T any] struct {
	Name     string // query-param name == JSON field == the allowlist key
	Kind     Kind
	Repeat   bool     // String: accept repeated keys, OR within the field
	MatchAll bool     // String+GetMulti+Repeat: require the item's set to contain ALL requested values (AND) instead of any (OR). For "supports both" filters like capability=.
	Enum     []string // String: if set, values must be one of these (else 400)
	Sortable bool     // may appear in ?sort=

	Get      func(*T) string
	GetMulti func(*T) []string
	GetBool  func(*T) bool
	GetInt   func(*T) int64
	GetTime  func(*T) time.Time
}

// Schema is a resource's complete filter contract: its filterable fields,
// an optional free-text corpus, and a default sort applied when ?sort= is
// absent.
type Schema[T any] struct {
	Fields []Field[T]
	// Q returns the free-text search corpus for one item; ?q= matches when
	// any corpus string contains the query (case-insensitive). Nil disables
	// the q param.
	Q func(*T) []string
	// Labels returns the item's label map; ?label=k=v (repeatable) matches
	// when every selector's key equals the given value (AND, like a k8s
	// label selector). Nil disables the label param.
	Labels func(*T) map[string]string
	// DefaultSort is the sort applied when ?sort= is absent, e.g. "name" or
	// "-created_at". Must reference a Sortable field; empty leaves input
	// order untouched.
	DefaultSort string
}

// Error is a rejected-request error (unknown key, bad value, disallowed
// sort). The HTTP layer maps it to 400 with Key naming the offending param.
type Error struct {
	Key string
	Msg string
}

func (e *Error) Error() string {
	if e.Key == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Key, e.Msg)
}

// MaxLimit caps ?limit= to bound response size on a hostile request.
const MaxLimit = 10_000

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
		q.limit = n
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

// Apply filters items by the compiled predicates + free-text, sorts the
// matches, then windows by offset/limit. total is the match count BEFORE
// windowing (for "N of M" displays). The input slice is not mutated.
func (q Query[T]) Apply(items []*T) (page []*T, total int) {
	matched := make([]*T, 0, len(items))
	for _, it := range items {
		if it == nil || !q.match(it) {
			continue
		}
		matched = append(matched, it)
	}
	total = len(matched)

	if q.sortField != nil {
		f := q.sortField
		sort.SliceStable(matched, func(i, j int) bool {
			return f.less(matched[i], matched[j]) != q.sortDesc
		})
	}

	lo := q.offset
	if lo > len(matched) {
		lo = len(matched)
	}
	hi := len(matched)
	if q.limit > 0 && lo+q.limit < hi {
		hi = lo + q.limit
	}
	return matched[lo:hi], total
}

func (q Query[T]) match(it *T) bool {
	for _, p := range q.preds {
		if !p(it) {
			return false
		}
	}
	if q.q != "" {
		hit := false
		for _, s := range q.schema.Q(it) {
			if strings.Contains(strings.ToLower(s), q.q) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(q.labels) > 0 {
		have := q.schema.Labels(it)
		for k, v := range q.labels {
			if have[k] != v {
				return false
			}
		}
	}
	return true
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

// compile turns one (key, suffix, values) tuple into a predicate, after
// validating it against the field's Kind. A nil predicate with nil error
// means "no constraint" (empty value).
func (f *Field[T]) compile(key, suffix string, vals []string) (func(*T) bool, error) {
	switch f.Kind {
	case String:
		if suffix != "" {
			return nil, &Error{Key: key, Msg: "range suffix not valid on a string field"}
		}
		wanted := nonEmpty(vals)
		if len(wanted) == 0 {
			return nil, nil
		}
		if len(wanted) > 1 && !f.Repeat {
			return nil, &Error{Key: key, Msg: "field is not repeatable"}
		}
		if f.Enum != nil {
			for _, w := range wanted {
				if !inSlice(f.Enum, w) {
					return nil, &Error{Key: key, Msg: fmt.Sprintf("invalid value %q (allowed: %s)", w, strings.Join(f.Enum, ", "))}
				}
			}
		}
		set := make(map[string]bool, len(wanted))
		for _, w := range wanted {
			set[w] = true
		}
		if f.GetMulti != nil {
			if f.MatchAll {
				// AND-membership: the item's set must contain every requested
				// value (e.g. capability=vision&capability=tools → supports both).
				return func(it *T) bool {
					have := make(map[string]bool, len(set))
					for _, h := range f.GetMulti(it) {
						have[h] = true
					}
					for w := range set {
						if !have[w] {
							return false
						}
					}
					return true
				}, nil
			}
			return func(it *T) bool {
				for _, have := range f.GetMulti(it) {
					if set[have] {
						return true
					}
				}
				return false
			}, nil
		}
		return func(it *T) bool { return set[f.Get(it)] }, nil

	case Bool:
		if suffix != "" {
			return nil, &Error{Key: key, Msg: "range suffix not valid on a boolean field"}
		}
		raw := vals[len(vals)-1]
		if raw == "" {
			return nil, nil
		}
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, &Error{Key: key, Msg: "must be true or false"}
		}
		return func(it *T) bool { return f.GetBool(it) == b }, nil

	case Int:
		if suffix != "_min" && suffix != "_max" {
			return nil, &Error{Key: key, Msg: "use " + f.Name + "_min / " + f.Name + "_max"}
		}
		raw := vals[len(vals)-1]
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, &Error{Key: key, Msg: "must be an integer"}
		}
		if suffix == "_min" {
			return func(it *T) bool { return f.GetInt(it) >= n }, nil
		}
		return func(it *T) bool { return f.GetInt(it) <= n }, nil

	case Time:
		if suffix != "_from" && suffix != "_to" {
			return nil, &Error{Key: key, Msg: "use " + f.Name + "_from / " + f.Name + "_to"}
		}
		raw := vals[len(vals)-1]
		if raw == "" {
			return nil, nil
		}
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, &Error{Key: key, Msg: "must be an RFC3339 timestamp"}
		}
		if suffix == "_from" {
			return func(it *T) bool {
				v := f.GetTime(it)
				return !v.IsZero() && !v.Before(ts)
			}, nil
		}
		return func(it *T) bool {
			v := f.GetTime(it)
			return !v.IsZero() && !v.After(ts)
		}, nil
	}
	return nil, &Error{Key: key, Msg: "unsupported field kind"}
}

// less reports whether a sorts before b on this field, ascending.
func (f *Field[T]) less(a, b *T) bool {
	switch f.Kind {
	case Int:
		return f.GetInt(a) < f.GetInt(b)
	case Time:
		return f.GetTime(a).Before(f.GetTime(b))
	case Bool:
		return !f.GetBool(a) && f.GetBool(b)
	default: // String — single-valued only (GetMulti is not orderable)
		return f.Get(a) < f.Get(b)
	}
}

func nonEmpty(vals []string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func inSlice(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
