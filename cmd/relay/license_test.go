package main

import (
	"sync"
	"testing"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settingswatch"
	"github.com/wyolet/relay/internal/license"
)

// fakeLicenseSource is the app/settingswatch.Source shape, reimplemented
// here (rather than imported from its test file) since that file lives in
// another package and is not exported.
type fakeLicenseSource struct {
	mu   sync.Mutex
	val  *settings.License
	subs []func()
}

func (f *fakeLicenseSource) Setting(string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val, f.val != nil
}

func (f *fakeLicenseSource) OnSettingsChange(_ string, fn func()) {
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	f.mu.Unlock()
}

func (f *fakeLicenseSource) change(v settings.License) {
	f.mu.Lock()
	f.val = &v
	subs := append([]func(){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

// TestApplyLicenseSection_ReachesServiceThroughTheWatcher proves the data
// path a stored license takes to become live: PUT /license only writes the
// settings row, and applyLicenseSection wired into a Watcher is what
// notices the change and calls Service.Set. Wrap the callback rather than
// call applyLicenseSection directly so the assertion is about the watcher
// invoking it, not just about the callback's own body.
//
// This build carries no release public key (internal/license.publicKey is
// the unreplaced empty placeholder, and that var is unexported so a test
// outside the license package cannot install a test key the way
// internal/license's own tests do). So a value can never verify here and
// Info().Licensed can never observably flip true — this test instead
// proves the watcher delivers each settings change to Service.Set
// (evidenced by the call count and the exact value forwarded, plus Set's
// rejection of a non-empty unverifiable value), which is the reachability
// contract in question.
func TestApplyLicenseSection_ReachesServiceThroughTheWatcher(t *testing.T) {
	svc := license.New(nil)
	src := &fakeLicenseSource{}

	var mu sync.Mutex
	var calls []settings.License
	base := applyLicenseSection(svc)
	spy := func(l settings.License) {
		mu.Lock()
		calls = append(calls, l)
		mu.Unlock()
		base(l)
	}

	w := settingswatch.New[settings.License](src, settings.SectionLicense, spy, nil)
	src.change(settings.License{}) // seed a value so Start's initial reconcile applies
	w.Start()

	mu.Lock()
	if len(calls) != 1 || calls[0].Value != "" {
		t.Fatalf("calls after Start = %+v, want one empty-value apply", calls)
	}
	mu.Unlock()
	if svc.Info().Licensed {
		t.Fatal("an empty license value must leave the service unlicensed")
	}

	// A later settings change — e.g. an admin pasting a license through the
	// API — must reach the same Service without any other wiring.
	src.change(settings.License{Value: "not-a-real-license"})

	mu.Lock()
	if len(calls) != 2 || calls[1].Value != "not-a-real-license" {
		t.Fatalf("calls after change = %+v, want the new value forwarded", calls)
	}
	mu.Unlock()
	// Unverifiable in this build (no release key), so it's rejected rather
	// than accepted — but it still reached Service.Set, which the rejection
	// itself demonstrates: an unreachable Set would leave nothing to reject.
	if svc.Info().Licensed {
		t.Fatal("an unverifiable license value must not report as licensed")
	}
}
