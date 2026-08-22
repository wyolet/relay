package usagelog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestInstanceStamp(t *testing.T) {
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now().UTC())
	pf := &lifecycle.PostFlightEvent{Status: 200}

	got, _ := NewUsageHook(nil, "mac-local").Fill(lc, pf)
	if ev := got.(*Event); ev.Extras[ExtrasKeyInstance] != "mac-local" {
		t.Fatalf("hook: extras %+v", ev.Extras)
	}

	// Existing relay-stamped extras survive alongside the instance key.
	lc.Metadata["client_ip"] = "10.0.0.1"
	got, _ = NewStreamUsageFactory(nil, "mac-local").NewObserver(lc).Result()
	if ev := got.(*Event); ev.Extras[ExtrasKeyInstance] != "mac-local" || ev.Extras["client_ip"] != "10.0.0.1" {
		t.Fatalf("stream: extras %+v", ev.Extras)
	}

	// Empty id: no key, existing deployments unchanged.
	got, _ = NewUsageHook(nil, "").Fill(lc, pf)
	if ev := got.(*Event); ev.Extras[ExtrasKeyInstance] != "" {
		t.Fatalf("empty id stamped: %+v", ev.Extras)
	}
}
