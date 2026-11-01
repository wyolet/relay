// Package orgwatch remembers, per pod and in memory, the provider organization each credential's served responses name, and reports an account whose credentials name different ones: those keys are separate organizations with separate upstream limits, which the account counts as one.
//
// It only reports, with one warning log line and one count of relay_account_org_mismatch_total per account and pair of organizations. It never moves a key or writes a row; detecting a key's account and moving it are the operator's actions on the control plane.
//
// Out of scope: sharing what one pod saw with the others, and forgetting deleted credentials (their entries stay until restart, bounded by the credentials the pod served).
package orgwatch

import (
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

// mismatches counts pairs of organizations found inside one account, once per pair.
var mismatches = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Name:      "account_org_mismatch_total",
		Help:      "Pairs of different provider organizations named by host keys of one account, counted once per account and pair per pod.",
	},
	[]string{"account"},
)

func init() { metrics.Register(mismatches) }

// Watch is the per-pod memory of the organization each credential last named. Safe for concurrent use.
type Watch struct {
	log *slog.Logger

	mu sync.RWMutex
	// last is what each credential's latest response named and under which account, the read every response makes.
	last map[string]seen
	// byAccount holds each account's credentials and the organization each named.
	byAccount map[string]map[string]string
	// reported holds the (account, pair) mismatches already logged and counted.
	reported map[mismatch]struct{}
}

type seen struct{ account, org string }

// mismatch is one account and two organizations, a < b.
type mismatch struct{ account, a, b string }

// New returns an empty Watch logging to log.
func New(log *slog.Logger) *Watch {
	return &Watch{log: log, last: map[string]seen{}, byAccount: map[string]map[string]string{}, reported: map[mismatch]struct{}{}}
}

// ObserveOrg records that credentialID, in accountID, was served under orgID. The repeat of what the credential last named takes a read lock and allocates nothing; anything new is compared against the account's other credentials.
func (w *Watch) ObserveOrg(accountID, credentialID, orgID string) {
	w.mu.RLock()
	s, ok := w.last[credentialID]
	w.mu.RUnlock()
	if ok && s.account == accountID && s.org == orgID {
		return
	}
	w.record(accountID, credentialID, orgID)
}

func (w *Watch) record(accountID, credentialID, orgID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.last[credentialID]; ok && s.account != accountID {
		// The credential moved: its old account no longer holds it.
		delete(w.byAccount[s.account], credentialID)
		if len(w.byAccount[s.account]) == 0 {
			delete(w.byAccount, s.account)
		}
	}
	w.last[credentialID] = seen{account: accountID, org: orgID}
	creds := w.byAccount[accountID]
	if creds == nil {
		creds = map[string]string{}
		w.byAccount[accountID] = creds
	}
	creds[credentialID] = orgID
	for other, otherOrg := range creds {
		if other == credentialID || otherOrg == orgID {
			continue
		}
		m := mismatch{account: accountID, a: orgID, b: otherOrg}
		if m.b < m.a {
			m.a, m.b = m.b, m.a
		}
		if _, done := w.reported[m]; done {
			continue
		}
		w.reported[m] = struct{}{}
		mismatches.WithLabelValues(accountID).Inc()
		w.log.Warn("orgwatch: keys of one account name different provider organizations; detect each key's account and move it",
			"account_id", accountID, "host_key_id", credentialID, "org", orgID, "other_host_key_id", other, "other_org", otherOrg)
	}
}
