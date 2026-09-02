// keys.go centralises the kv keys this package's Reserve call builds. The
// limiter renders a rule's key as "limit:{<scope>}:<rule key>...", so the
// scope is the hash tag every counter of one request shares: the team once
// the caller has a project, the policy slug otherwise (a personal key has no
// team to anchor on).
package policy

import "fmt"

// reserveScope returns the inbound Reserve hash tag. Anchoring on the team
// keeps a project's rate-limit buckets, its revocation denylist and (later)
// its budget counters in one Redis Cluster slot, which is what lets them all
// ride the single Reserve script.
func reserveScope(teamID, policySlug string) string {
	if teamID != "" {
		return "team:" + teamID
	}
	return policySlug
}

// ruleSubject renders the bucket family a policy's rate rules count in. The
// limiter turns it into "limit:{<scope>}:policy:<subject>:<rule index>:<meter>",
// so the full key is:
//
//	limit:{<scope>}:policy:<policy slug>:rl:<rateLimit id>[:m:<model id>]:<i>:<meter>
//
// The RateLimit id is in the key because one policy can carry several through
// RLBindings, and the model id because two bindings meter different models —
// without both, every per-model binding shared one bucket. modelID is empty
// for the flat Spec.RateLimitID, whose single bucket spans every model.
// One concatenation, not two: this runs per rule per request.
func ruleSubject(policySlug, rateLimitID, modelID string) string {
	if modelID == "" {
		return policySlug + ":rl:" + rateLimitID
	}
	return policySlug + ":rl:" + rateLimitID + ":m:" + modelID
}

// revokedRuleKey returns the rule key for a token's revocation check; the
// limiter prepends "limit:{<scope>}:" to it.
func revokedRuleKey(jti string) string { return "jti:" + jti }

// RevokedKey is the full kv key whose existence revokes one token. Revoking
// writes it with a TTL of the token's remaining life; Reserve reads it as
// the MeterRevoked rule.
func RevokedKey(teamID, jti string) string {
	return fmt.Sprintf("limit:{%s}:%s", reserveScope(teamID, ""), revokedRuleKey(jti))
}
