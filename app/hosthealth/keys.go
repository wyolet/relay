package hosthealth

// healthKey returns the kv key for a host's runtime health record.
// format: host_health:{host:<hostID>}
// Hash tag {host:<hostID>} keeps per-host state on a single Cluster slot;
// health ops are single-key and need no cross-slot alignment.
func healthKey(hostID string) string {
	return "host_health:{host:" + hostID + "}"
}

// hostIDFromKey inverts healthKey; returns "" for keys not in its format.
func hostIDFromKey(key string) string {
	const pre = "host_health:{host:"
	if len(key) <= len(pre)+1 || key[:len(pre)] != pre || key[len(key)-1] != '}' {
		return ""
	}
	return key[len(pre) : len(key)-1]
}
