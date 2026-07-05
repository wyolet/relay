package jobq

import (
	"testing"
	"time"
)

func TestOptionsWithDefaults_ZeroValueIsUsable(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Options{}.withDefaults() panicked: %v", r)
		}
	}()

	got := (Options{}).withDefaults()
	if got.RescueAfter <= got.JobTimeout {
		t.Fatalf("RescueAfter = %v, JobTimeout = %v; want RescueAfter > JobTimeout", got.RescueAfter, got.JobTimeout)
	}
}

func TestOptionsWithDefaults_PanicsWhenRescueAfterDoesNotExceedJobTimeout(t *testing.T) {
	mustPanic(t, func() {
		Options{JobTimeout: time.Minute, RescueAfter: time.Minute}.withDefaults()
	})
	mustPanic(t, func() {
		Options{JobTimeout: 2 * time.Minute, RescueAfter: time.Minute}.withDefaults()
	})
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("function returned without panic")
		}
	}()
	fn()
}
