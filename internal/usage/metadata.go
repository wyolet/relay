package usage

import (
	"strings"
	"sync/atomic"
)

// Rejection reason labels for relay_metadata_rejected_total.
const (
	ReasonOversize   = "oversize"
	ReasonBadCharset = "bad_charset"
	ReasonMalformed  = "malformed"
)

var (
	rejectedOversize   atomic.Uint64
	rejectedBadCharset atomic.Uint64
	rejectedMalformed  atomic.Uint64
)

// MetadataRejected returns the cumulative count for the given reason label.
// Valid reason values are ReasonOversize, ReasonBadCharset, ReasonMalformed.
func MetadataRejected(reason string) uint64 {
	switch reason {
	case ReasonOversize:
		return rejectedOversize.Load()
	case ReasonBadCharset:
		return rejectedBadCharset.Load()
	case ReasonMalformed:
		return rejectedMalformed.Load()
	}
	return 0
}

func incRejected(reason string) {
	switch reason {
	case ReasonOversize:
		rejectedOversize.Add(1)
		metricMetadataRejectedOversize.Inc()
	case ReasonBadCharset:
		rejectedBadCharset.Add(1)
		metricMetadataRejectedBadCharset.Inc()
	case ReasonMalformed:
		rejectedMalformed.Add(1)
		metricMetadataRejectedMalformed.Inc()
	}
}

// ParseMetadataHeader parses the X-Relay-Metadata header value.
// Format: comma-separated k=v pairs; whitespace trimmed around each token.
// Limits: max 16 pairs, max 64-char keys ([a-zA-Z0-9_.-]), max 256-char values
// (printable ASCII 0x20..0x7E excluding ',' and '=').
//
// Any single violation drops the entire header (nil returned, request continues).
// Increments relay_metadata_rejected_total{reason=...} with the first failure's reason:
//   - oversize: pair count > 16, key > 64 chars, or value > 256 chars
//   - bad_charset: key contains invalid character, or value contains ',' or '=' or non-printable
//   - malformed: pair has no '=' separator
//
// Returns nil on empty input (no allocation).
func ParseMetadataHeader(headerValue string) map[string]string {
	if headerValue == "" {
		return nil
	}

	pairs := strings.Split(headerValue, ",")
	if len(pairs) > 16 {
		incRejected(ReasonOversize)
		return nil
	}

	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			incRejected(ReasonMalformed)
			return nil
		}
		k := strings.TrimSpace(pair[:idx])
		v := strings.TrimSpace(pair[idx+1:])

		if len(k) > 64 || len(v) > 256 {
			incRejected(ReasonOversize)
			return nil
		}
		if !validKey(k) {
			incRejected(ReasonBadCharset)
			return nil
		}
		if !validValue(v) {
			incRejected(ReasonBadCharset)
			return nil
		}
		out[k] = v
	}
	return out
}

func validKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || c == ',' || c == '=' {
			return false
		}
	}
	return true
}
