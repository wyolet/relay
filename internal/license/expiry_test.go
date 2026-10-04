package license

import (
	"testing"
	"time"

	applicense "github.com/wyolet/relay/app/license"
)

// A licence verified at boot must stop unlocking features once its grace
// window closes — a gateway that runs for months would otherwise honour an
// expired licence until someone restarts it.
func TestHasReevaluatesExpiryOnEveryCall(t *testing.T) {
	now := time.Now().UTC()
	clock := now
	s := New(func() time.Time { return clock })
	s.cur.Store(&License{
		Customer:  "acme",
		ExpiresAt: now.Add(24 * time.Hour),
		Features:  []string{applicense.FeatureSSO},
	})

	if !s.Has(applicense.FeatureSSO) {
		t.Fatal("a live licence must unlock its features")
	}
	// Inside the grace window the licence keeps working.
	clock = now.Add(48 * time.Hour)
	if !s.Has(applicense.FeatureSSO) {
		t.Error("an expired licence inside the grace window must keep working")
	}
	clock = now.Add(24*time.Hour + GraceWindow + time.Minute)
	if s.Has(applicense.FeatureSSO) {
		t.Error("a licence past its grace window must unlock nothing")
	}
}
