package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// A rotated key's previous hash is indexed only while its grace window is
// open. With an injected clock the transition is reachable without sleeping.
func TestGraceWindowIndexUsesTheCatalogClock(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	k.Spec.KeyHash = hex64("c")
	k.Spec.PreviousKeyHash = hex64("d")
	until := base.Add(time.Hour)
	k.Spec.GraceUntil = &until

	c := New(provList{}, hostList{}, polList{}, modList{}, keyList{}, rlList{},
		rkList{k}, rcList{}, bndList{})
	c.UseClock(func() time.Time { return now })
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, prev := c.Current().KeyByHash(hex64("d")); got == nil || !prev {
		t.Fatal("previous hash is not indexed inside the grace window")
	}

	now = until.Add(time.Second)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := c.Current().KeyByHash(hex64("d")); got != nil {
		t.Fatal("previous hash still indexed after the grace window closed")
	}
}
