package otlp

import (
	"math"
	"strconv"
)

// Attrs is a set of OpenTelemetry attributes with values converted to Go types: string, bool, int64, float64, []byte, []any and map[string]any.
//
// The accessors take several keys because conventions rename attributes between releases; the first key that holds a usable value wins.
type Attrs map[string]any

// Str returns the first non-empty string among keys.
func (a Attrs) Str(keys ...string) string {
	for _, k := range keys {
		if s, ok := a[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Int returns the first integer among keys. Conventions type counts as int, but exporters also send them as whole doubles or numeric strings.
func (a Attrs) Int(keys ...string) (int64, bool) {
	for _, k := range keys {
		switch v := a[k].(type) {
		case int64:
			return v, true
		case float64:
			if v == math.Trunc(v) && math.Abs(v) < 1<<62 {
				return int64(v), true
			}
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// Float returns the first number among keys.
func (a Attrs) Float(keys ...string) (float64, bool) {
	for _, k := range keys {
		switch v := a[k].(type) {
		case float64:
			return v, true
		case int64:
			return float64(v), true
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// Bool returns the first boolean among keys.
func (a Attrs) Bool(keys ...string) (value, ok bool) {
	for _, k := range keys {
		if b, isBool := a[k].(bool); isBool {
			return b, true
		}
	}
	return false, false
}

// Strings returns the first string array among keys, skipping elements that are not strings. A lone string counts as a one-element array.
func (a Attrs) Strings(keys ...string) []string {
	for _, k := range keys {
		switch v := a[k].(type) {
		case []any:
			out := make([]string, 0, len(v))
			for _, e := range v {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if v != "" {
				return []string{v}
			}
		}
	}
	return nil
}
