// Diagnostics on catalog rows: a model no binding can serve, and a disabled host that pools or keys still point at. They attach issues and build nothing the request path reads.

package plan

import (
	"fmt"
	"slices"
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/engine"
)

// reportUnservableModels files model_unservable on each enabled model whose bindings, of which it has at least one, are all disabled or at hosts requests are not routed to.
func reportUnservableModels(tx *engine.Txn, snap *appcatalog.Snapshot, rows []*model.Model) {
	for _, row := range rows {
		m, ok := snap.Model(row.Meta.ID)
		if !ok || !m.IsEnabled() {
			continue
		}
		bs := snap.BindingsForModel(m.Meta.ID)
		if len(bs) == 0 {
			continue
		}
		served := false
		var hosts []string
		for _, b := range bs {
			h, ok := snap.Host(b.Spec.HostID)
			if !ok {
				continue
			}
			if b.IsEnabled() && servesRequests(h) {
				served = true
				break
			}
			hosts = append(hosts, h.Meta.Name)
		}
		if served {
			continue
		}
		msg := "no enabled binding at an enabled host serves it; requests for it are refused with no_binding"
		if len(hosts) > 0 {
			slices.Sort(hosts)
			msg = "no enabled binding at an enabled host serves it (its bindings are at " + strings.Join(slices.Compact(hosts), ", ") + "); requests for it are refused with no_binding"
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssueModelUnservable, Source: engine.Ref{Kind: KindModel, ID: m.Meta.ID}, Message: msg})
	}
}

// hostCheck is the kind of the host checks' output, which they never build: a check only attaches issues to the host.
type hostCheck struct{}

// poolHost and credentialHost are a pool's and a credential's Watch keys into the host checks: their host.
func poolHost(p *pool.Pool) []string             { return []string{p.Spec.HostID} }
func credentialHost(k *hostkey.HostKey) []string { return []string{k.Spec.HostID} }

func (c *controllers) hostCheck(tx *engine.Txn, id string) error {
	if h := engine.Get[host.Host](tx, id); h != nil && !h.IsEnabled() {
		pools := len(engine.Members[pool.Pool](tx))
		keys := 0
		for _, k := range engine.Members[hostkey.HostKey](tx) {
			if k.IsEnabled() {
				keys++
			}
		}
		if pools+keys > 0 {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueHostDisabledInUse, Source: engine.Ref{Kind: KindHost, ID: id},
				Message: fmt.Sprintf("the host is disabled; %s and %s point at it and serve nothing", count(pools, "pool"), count(keys, "enabled key"))})
		}
	}
	engine.Drop(tx)
	return nil
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
