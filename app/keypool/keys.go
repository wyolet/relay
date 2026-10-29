package keypool

// circuitKey returns the Redis key for a secret's circuit-breaker record.
// format: secret_health:{secret:<keyHash>}
// Hash tag {secret:<keyHash>} groups circuit state per secret on a single
// Cluster slot; circuit-breaker ops are single-key and need no cross-slot alignment.
func circuitKey(keyHash string) string {
	return "secret_health:{secret:" + keyHash + "}"
}

// roundRobinKey returns the Redis key for a pick scope's round-robin counter.
// format: pool_rr:{pool:<scope>}
// The scope is the caller's stable id (the policy id), so renaming the policy keeps its rotation.
func roundRobinKey(scope string) string {
	return "pool_rr:{pool:" + scope + "}"
}

// lruKey returns the Redis key used to store the last-use timestamp for a key within a pick scope.
// format: {pool_lru:<scope>}:<keyHash>
func lruKey(scope, keyHash string) string {
	return "{pool_lru:" + scope + "}:" + keyHash
}
