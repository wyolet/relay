package filter

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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
		if len(vals) == 0 {
			return nil, nil
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
		if len(vals) == 0 {
			return nil, nil
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
		if len(vals) == 0 {
			return nil, nil
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
