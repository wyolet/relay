package main

import "strings"

// maskValue returns a masked representation of a cleartext key value.
func maskValue(cleartext string) string {
	if len(cleartext) == 0 {
		return "***"
	}
	last4 := cleartext
	if len(cleartext) > 4 {
		last4 = cleartext[len(cleartext)-4:]
	}
	prefixes := []string{"sk-", "gsk_", "xai-", "ant-", "hf_"}
	for _, p := range prefixes {
		if strings.HasPrefix(cleartext, p) {
			return p + "..." + last4
		}
	}
	return "***..." + last4
}
