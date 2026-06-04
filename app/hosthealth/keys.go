package hosthealth

// healthKey returns the kv key for a host's runtime health record.
// format: host_health:{host:<hostID>}
// Hash tag {host:<hostID>} keeps per-host state on a single Cluster slot;
// health ops are single-key and need no cross-slot alignment.
func healthKey(hostID string) string {
	return "host_health:{host:" + hostID + "}"
}
