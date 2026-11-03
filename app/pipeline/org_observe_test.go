package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/orgwatch"
)

// orgAdapter answers 200 naming the organization in X-Org.
type orgAdapter struct{ reservationTestAdapter }

func (orgAdapter) Call(context.Context, string, *string, string, []byte, http.Header, string, bool, bool) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{"X-Org": {"org-1"}}}, nil
}

func (orgAdapter) OrgID(h http.Header) string {
	if v := h["X-Org"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

type orgRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *orgRecorder) ObserveOrg(accountID, credentialID, orgID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, accountID+"/"+credentialID+"/"+orgID)
}

// A served response's organization reaches the observer under the credential's account; an adapter that reads none reports nothing.
func TestRunObservesTheServedOrg(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		adapter Adapter
		want    string
	}{
		{"reads", orgAdapter{}, "acct-1/host-key-1/org-1"},
		{"plain", reservationTestAdapter{}, ""},
	} {
		h := newReservationHarness()
		rec := &orgRecorder{}
		h.pipeline.Orgs = rec
		res, err := h.pipeline.Run(context.Background(), &Request{Adapter: tc.adapter, Hops: h.hops})
		if err != nil {
			t.Fatalf("%s: Run: %v", tc.name, err)
		}
		_ = res.Body.Close()
		got := strings.Join(rec.seen, ",")
		if got != tc.want {
			t.Fatalf("%s: observed %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The read runs on every served response: once the credential's org is known, it allocates nothing.
func TestObserveOrgAllocatesNothingOnARepeat(t *testing.T) {
	h := newReservationHarness()
	p := &Pipeline{Orgs: orgwatch.New(slog.New(slog.DiscardHandler))}
	slot := h.hops[0].Slots[0]
	hdr := http.Header{"X-Org": {"org-1"}}
	var a Adapter = orgAdapter{}
	p.observeOrg(a, slot, hdr)
	if n := testing.AllocsPerRun(100, func() { p.observeOrg(a, slot, hdr) }); n != 0 {
		t.Fatalf("observeOrg allocs = %v, want 0", n)
	}
}
