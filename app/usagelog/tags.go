package usagelog

import (
	"encoding/json"

	"github.com/wyolet/relay/pkg/usage"
)

// MetadataKeyRequestTags is the lifecycle Metadata key under which the
// inference edge stashes the raw X-WR-Request-Tags header value. The hot
// path copies the string only; ParseTags runs post-flight.
const MetadataKeyRequestTags = "request_tags_raw"

// MaxTagsHeaderBytes caps the raw header at the inference edge — longer
// values are never copied into Metadata.
const MaxTagsHeaderBytes = 2048

const (
	maxTagCount    = 16
	maxTagValueLen = 256
)

// ParseTags validates a raw X-WR-Request-Tags value: a flat JSON object
// with string values only, ≤16 keys, key ≤64 chars, value ≤256 chars.
// Any violation drops the whole blob (nil, false) — caller tags are
// best-effort observability and must never affect the request.
func ParseTags(raw string) (map[string]string, bool) {
	if raw == "" || len(raw) > MaxTagsHeaderBytes {
		return nil, false
	}
	// Decode via `any` values: unmarshalling straight into map[string]string
	// silently maps JSON null to "" instead of rejecting it.
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, false
	}
	if len(obj) == 0 || len(obj) > maxTagCount {
		return nil, false
	}
	tags := make(map[string]string, len(obj))
	for k, v := range obj {
		s, ok := v.(string)
		if !ok || k == "" || len(k) > usage.MaxTagKeyLen || len(s) > maxTagValueLen {
			return nil, false
		}
		tags[k] = s
	}
	return tags, true
}
