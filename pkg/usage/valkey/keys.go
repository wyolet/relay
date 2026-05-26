package valkey

import "time"

// All event keys share the hash-tag "{u}" so a Redis Cluster Range prefix
// scan lands on one slot. The timestamp is zero-padded to 20 digits so
// lexical key order approximates time order (not relied upon — we sort in Go).
//
// Full key: <prefix>:{u}:<20-digit-unixnano>:<request_id>
// Scan prefix: <prefix>:{u}:

func eventKey(prefix string, ts time.Time, requestID string) string {
	return prefix + ":{u}:" + zeroPadNano(ts) + ":" + requestID
}

// scanPrefix is the common prefix for Range when listing all events.
func scanPrefix(prefix string) string {
	return prefix + ":{u}:"
}

// zeroPadNano formats t.UnixNano() as a 20-digit zero-padded decimal string.
// 20 digits covers the year-2262 epoch-ns ceiling safely.
func zeroPadNano(t time.Time) string {
	const width = 20
	n := t.UnixNano()
	// Format without allocation using a fixed-width byte array.
	var buf [width]byte
	for i := width - 1; i >= 0; i-- {
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[:])
}
