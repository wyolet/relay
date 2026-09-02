package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/policybinding"
)

func rejectionCount(t *testing.T, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := authRejectedTotal.GetMetricWithLabelValues(reason)
	if err != nil {
		t.Fatalf("metric: %v", err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// An unauthenticated flood must not amplify into the log pipeline: the
// rejection is a counter, and the log line drops to Debug.
func TestAuthRejectionsAreCounted(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-wr-live"))

	before := rejectionCount(t, "invalid api key")
	for i := 0; i < 3; i++ {
		if w := st.do("sk-wr-nope"); w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	}
	if got := rejectionCount(t, "invalid api key") - before; got != 3 {
		t.Fatalf("counter rose by %v, want 3", got)
	}
}

func TestForbiddenRejectionsAreCountedByCode(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = nil
	st := f.stack(t, saKey(f, "sk-wr-live"))

	before := rejectionCount(t, "no_policy")
	// The service account's own policy is what normally resolves; drop it
	// so the principal reaches the no-policy refusal.
	f.sa.Spec.PolicyID = ""
	if err := st.cat.ApplyServiceAccountUpsert(f.sa); err != nil {
		t.Fatalf("apply sa: %v", err)
	}
	if w := st.do("sk-wr-live"); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := rejectionCount(t, "no_policy") - before; got != 1 {
		t.Fatalf("counter rose by %v, want 1", got)
	}
}

// One request reads one snapshot: the auth middleware pins it on ctx and
// everything downstream reads that same pointer, so a reload landing
// mid-request cannot split a request across two views.
func TestSnapshotIsPinnedOnTheRequestContext(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	k := saKey(f, "sk-wr-live")

	c := f.stack(t, k)
	var seen *snapshotProbe
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = &snapshotProbe{fromCtx: SnapshotFrom(r.Context())}
		// A reload between the middleware and the handler must not change
		// what the handler reads.
		if err := c.cat.Reload(r.Context()); err != nil {
			t.Errorf("reload: %v", err)
		}
		seen.live = c.cat.Current()
		w.WriteHeader(http.StatusOK)
	})
	h := ClassifyMiddleware()(PrincipalMiddleware(c.cat, f.tokens)(inner))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-wr-live")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil || seen.fromCtx == nil {
		t.Fatal("no snapshot on the request context")
	}
	if seen.fromCtx == seen.live {
		t.Fatal("the request read the live snapshot, not the one it authenticated against")
	}
}

type snapshotProbe struct {
	fromCtx, live *appcatalog.Snapshot
}
