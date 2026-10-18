package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

type recordingKeys struct {
	changed  []string
	reloaded int
}

func (r *recordingKeys) KeyChanged(_ context.Context, id string) error {
	r.changed = append(r.changed, id)
	return nil
}

func (r *recordingKeys) ReloadKeys(context.Context) error {
	r.reloaded++
	return nil
}

// Data-key and target-KEK notifications reach the key ring, on the incremental path and inside a bulk burst that falls back to a full reload.
func TestSecretKeyEventsReachTheKeyRing(t *testing.T) {
	for _, payload := range []string{"secretkey:upsert:0192f3a0-0000-7000-8000-000000000001", "secretkey:upsert:target", "secretkey:delete:0192f3a0-0000-7000-8000-000000000001"} {
		if _, ok := parseEvent(payload); !ok {
			t.Fatalf("parseEvent(%q) refused a secretkey payload", payload)
		}
	}
	for _, burst := range []int{0, reloadBatchThreshold} {
		keys := &recordingKeys{}
		c := tenancyCatalog(t, nil, nil, nil, nil, nil)
		l := &Listener{cat: c, deb: newDebouncer(0), stores: listenerStores{secretKeys: keys}}
		for i := 0; i < burst; i++ {
			l.deb.push(notifyEvent{Kind: "team", Op: "delete", ID: meta.NewID()})
		}
		l.deb.push(notifyEvent{Kind: "secretkey", Op: "upsert", ID: "target"})
		l.applyDrained(context.Background())
		if len(keys.changed) != 1 || keys.changed[0] != "target" {
			t.Fatalf("burst of %d: key ring saw %v, want [target]", burst, keys.changed)
		}
	}
}
