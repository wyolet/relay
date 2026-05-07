package keypool

// circuitKey returns the Redis key for a secret's circuit-breaker record.
// format: secret_health:{secret:<keyHash>}
// Hash tag {secret:<keyHash>} groups circuit state per secret on a single
// Cluster slot; circuit-breaker ops are single-key and need no cross-slot alignment.
func circuitKey(keyHash string) string {
	return "secret_health:{secret:" + keyHash + "}"
}

// roundRobinKey returns the Redis key for a pool's round-robin counter.
// format: pool_rr:{pool:<poolName>}
// Hash tag {pool:<poolName>} aligns with pkg/limit keys so all pool-scoped
// state can land on the same Cluster slot if needed.
func roundRobinKey(poolName string) string {
	return "pool_rr:{pool:" + poolName + "}"
}
