package plan_test

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/meta"
)

// A writer waiting on a lane returns once a publish carries it, and a closed lane is not open; with nothing published it gives up when its context ends.
func TestHolderAwaitOpenLane(t *testing.T) {
	var w plantest.World
	u := w.User()
	proj := w.Project(w.Team())
	plans := w.Load(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	member := account.Member(u.ID, u.Username, meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}, proj.Meta.Name, "")
	member.Meta.ID = meta.NewID()
	member.Spec.Closed = true
	done := make(chan bool, 1)
	go func() { done <- plans.Holder.AwaitOpenLane(ctx, member.Meta.ID) }()
	plantest.Apply(t, plans, member)
	select {
	case <-done:
		t.Fatal("a closed lane counted as open")
	case <-time.After(50 * time.Millisecond):
	}
	open := *member
	open.Spec.Closed = false
	plantest.Apply(t, plans, &open)
	if !<-done {
		t.Fatal("the wait ended without the published lane")
	}

	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if plans.Holder.AwaitOpenLane(short, meta.NewID()) {
		t.Fatal("a lane no publish carries counted as served")
	}
}
