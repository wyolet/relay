package inference

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/reqid"
)

// Frames of one WebSocket connection run concurrently over the upgrade's
// context. Each must get its own request state, and none may write to the
// upgrade's: under -race a shared state would be reported here.
func TestConcurrentWSFramesNeverShareState(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)

	upgrade := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	upgrade = fillPrincipal(upgrade.WithContext(WithClassification(upgrade.Context(), Classification{Mode: ModeNormal})), cat.Current(), pr, pr.Key)
	shared := stateFrom(upgrade.Context())
	sharedCopy := *shared

	const frames = 32
	states := make([]*requestState, frames)
	var wg sync.WaitGroup
	for i := range frames {
		wg.Go(func() {
			fr := upgrade.Clone(reqid.WithNewID(upgrade.Context(), slog.Default()))
			w := httptest.NewRecorder()
			got, ok := frameRequest(d, w, fr)
			if !ok {
				t.Errorf("frame %d rejected: %d %s", i, w.Code, w.Body)
				return
			}
			states[i] = stateFrom(got.Context())
			if PrincipalFrom(got.Context()) == pr || ClassificationFrom(got.Context()).Mode != ModeNormal {
				t.Errorf("frame %d: principal or classification not the frame's own", i)
			}
		})
	}
	wg.Wait()

	seen := map[*requestState]bool{shared: true}
	for i, s := range states {
		if s == nil {
			continue
		}
		if seen[s] {
			t.Fatalf("frame %d shares a request state with the upgrade or another frame", i)
		}
		seen[s] = true
	}
	if *shared != sharedCopy {
		t.Fatalf("a frame changed the upgrade's request state")
	}
}

// Contexts built without the middleware chain carry the per-value keys; the
// accessors keep reading them where no state sets the value.
func TestAccessorsFallBackToValueKeys(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	snap := cat.Current()

	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, pr)
	ctx = context.WithValue(ctx, ctxKeyT{}, pr.Key)
	ctx = context.WithValue(ctx, ctxSnapshotT{}, snap)
	ctx = WithClassification(ctx, Classification{Mode: ModeNormal})

	if PrincipalFrom(ctx) != pr || KeyFromContext(ctx) != pr.Key || SnapshotFrom(ctx) != snap {
		t.Fatalf("accessors lost the values set without the middleware")
	}
	if ClassificationFrom(ctx).Mode != ModeNormal {
		t.Fatalf("classification = %v, want ModeNormal", ClassificationFrom(ctx).Mode)
	}
	// A nil snapshot pinned on top hides the outer one, as a nested value did.
	if got := SnapshotFrom(WithSnapshot(ctx, nil)); got != nil {
		t.Fatalf("SnapshotFrom after pinning nil = %p, want nil", got)
	}
	if ClassificationFrom(context.Background()) != (Classification{}) || PrincipalFrom(context.Background()) != nil {
		t.Fatalf("a bare context must read as unclassified and anonymous")
	}
}

// The context writers derive a child state; the parent's stays as it was.
func TestStateWritersCopyOnWrite(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	parent := withPrincipal(WithClassification(context.Background(), Classification{Mode: ModeNormal, Key: "k"}), cat.Current(), pr, pr.Key)

	child := WithClassification(WithSnapshot(parent, nil), Classification{Mode: ModeProxyAuthed})

	if ClassificationFrom(parent).Mode != ModeNormal || SnapshotFrom(parent) != cat.Current() {
		t.Fatalf("a child writer changed the parent's state")
	}
	if ClassificationFrom(child).Mode != ModeProxyAuthed || SnapshotFrom(child) != nil || PrincipalFrom(child) != pr {
		t.Fatalf("child state = %+v, want the parent's principal with its own classification and snapshot", *stateFrom(child))
	}
}

type namedProfile struct{ clientprofile.Profile }

func (namedProfile) Name() string { return "named" }

func TestRouteProfileSetOnlyWhenDifferent(t *testing.T) {
	p := namedProfile{clientprofile.Default}
	r := httptest.NewRequest(http.MethodPost, "/named/v1/messages", nil)

	set := withRouteProfile(r, p)
	if set == r || clientprofile.FromContext(set.Context()).Name() != "named" {
		t.Fatalf("route profile not set on a request without one")
	}
	if again := withRouteProfile(set, p); again != set {
		t.Fatalf("route profile set again over the same profile")
	}
}
