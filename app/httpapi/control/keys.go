// keys.go is the control plane's kv key vocabulary. Only the token-mint
// window lives here today; every kv key this package writes belongs in this
// file so the hash tags stay greppable from one place.
//
// Expected kv ops per mint: one Reserve (a single script call).
package control

import "time"

// mintLimitWindow and mintLimitBudget bound how often one user may mint an
// inference token. A mint is a session-authenticated write that signs a
// long-lived credential; the cap is a floor under abuse, not a quota.
const (
	mintLimitWindow = time.Minute
	mintLimitBudget = 30
)

// mintLimitRule is the rule segment of the mint window's kv keys.
const mintLimitRule = "mint"

// mintLimitScope is the hash tag every key of one user's mint window shares.
// pkg/ratelimit renders the fixed-window key as
// limit:{<scope>}:fw:<rule>:requests:<bucket>.
func mintLimitScope(userID string) string { return "user:" + userID }

// mintLimitKeyPrefix is what the limiter writes for that window, up to the
// bucket timestamp. Nothing constructs kv keys from it — it exists so the
// rendered shape is asserted in one test rather than inferred.
func mintLimitKeyPrefix(userID string) string {
	return "limit:{" + mintLimitScope(userID) + "}:fw:" + mintLimitRule + ":requests:"
}
