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
	// DefaultLimit is the page size applied when ?limit= is absent, so a
	// bare list request can't return an unbounded set. 0 keeps the legacy
	// return-everything behavior. An explicit ?limit=0 opts out (returns
	// everything up to MaxLimit semantics); the response's pre-window total
	// always reports the full match count.
	DefaultLimit int
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
