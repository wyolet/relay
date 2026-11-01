package orgwatch

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestWatch() (*Watch, *bytes.Buffer) {
	var buf bytes.Buffer
	return New(slog.New(slog.NewTextHandler(&buf, nil))), &buf
}

func counted(account string) float64 {
	return testutil.ToFloat64(mismatches.WithLabelValues(account))
}

// Two keys of one account naming different orgs are reported once, however often they keep doing so; a third org is a new pair.
func TestObserveOrgReportsAMismatchOnce(t *testing.T) {
	w, logs := newTestWatch()
	acct := "acct-once"
	w.ObserveOrg(acct, "k1", "org-a")
	w.ObserveOrg(acct, "k2", "org-a")
	if counted(acct) != 0 {
		t.Fatal("keys naming one org were reported")
	}
	for range 3 {
		w.ObserveOrg(acct, "k2", "org-b")
		w.ObserveOrg(acct, "k1", "org-a")
		w.ObserveOrg(acct, "k1", "org-b")
		w.ObserveOrg(acct, "k1", "org-a")
	}
	if got := counted(acct); got != 1 {
		t.Fatalf("mismatches = %v, want 1", got)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", n, logs)
	}
	w.ObserveOrg(acct, "k3", "org-c")
	if got := counted(acct); got != 3 {
		t.Fatalf("mismatches after a third org = %v, want 3 (org-c against org-a and org-b)", got)
	}
}

// A key that moves to another account stops counting against the old one.
func TestObserveOrgForgetsAMovedKey(t *testing.T) {
	w, _ := newTestWatch()
	w.ObserveOrg("acct-old", "k1", "org-a")
	w.ObserveOrg("acct-new", "k1", "org-a")
	w.ObserveOrg("acct-old", "k2", "org-b")
	if got := counted("acct-old"); got != 0 {
		t.Fatalf("old account mismatches = %v, want 0 once the key left", got)
	}
}

func TestObserveOrgRepeatAllocatesNothing(t *testing.T) {
	w, _ := newTestWatch()
	w.ObserveOrg("acct-allocs", "k1", "org-a")
	if n := testing.AllocsPerRun(100, func() { w.ObserveOrg("acct-allocs", "k1", "org-a") }); n != 0 {
		t.Fatalf("allocs = %v, want 0", n)
	}
}
