package keypool

// Verdict is what a key agent tells the request to do after a key failed. It lives here, beside FailureKind, so the agent and the request loop share it without importing each other.
type Verdict int

const (
	// VerdictNext: give up on this key, fail over to the next candidate.
	VerdictNext Verdict = iota
	// VerdictRetry: retry the SAME key with the returned fresh secret value
	// (the agent re-resolved it — e.g. the key had rotated upstream).
	VerdictRetry
	// VerdictFail: stop; return the upstream error to the caller.
	VerdictFail
)
