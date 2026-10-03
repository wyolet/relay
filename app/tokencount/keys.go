package tokencount

import "strings"

// The hash tag is the package's own name rather than the session or model id: the two ratios of one lookup are read together, so keeping the whole namespace on one Cluster slot lets a future batched read stay single-slot.
const (
	sessionPrefix = "{tokencount}:session:"
	modelPrefix   = "{tokencount}:model:"
)

// sessionKey returns the kv key for one conversation's observed ratio.
func sessionKey(id string) string { return sessionPrefix + id }

// modelKey returns the kv key for one model's observed ratio.
func modelKey(id string) string { return modelPrefix + id }

// maxIDLen bounds an id embedded in a key. Session ids arrive from a request header, so their length and alphabet are the caller's choice, not ours.
const maxIDLen = 128

// safeID returns id when it can be embedded in a kv key verbatim, else "". Rejecting rather than escaping keeps the stored key readable and makes a hostile id a no-op instead of a namespace collision.
func safeID(id string) string {
	if id == "" || len(id) > maxIDLen {
		return ""
	}
	if strings.ContainsAny(id, "{}:") {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] == 0x7f {
			return ""
		}
	}
	return id
}
