package keypool

import (
	"regexp"
	"testing"
)

// Each key carries exactly one hash tag, so a consumer's keys share a Cluster slot.
func TestKeys_FormatAndHashTag(t *testing.T) {
	oneTag := regexp.MustCompile(`^[^{}]*\{[^{}]+\}[^{}]*$`)
	for got, want := range map[string]string{
		circuitKey("sha256abc"):      "secret_health:{secret:sha256abc}",
		roundRobinKey("policy-1"):    "pool_rr:{pool:policy-1}",
		lruKey("policy-1", "sha256"): "{pool_lru:policy-1}:sha256",
	} {
		if got != want || !oneTag.MatchString(got) {
			t.Errorf("key = %q, want %q with one hash tag", got, want)
		}
	}
}
