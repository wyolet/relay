// Diagnostics on catalog rows: the models no binding can serve, and a disabled host that pools or keys still point at. They attach issues and build nothing the request path reads.

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

// unservableModel is an enabled model with bindings, none of them enabled at a host requests are routed to; hosts names its bindings' hosts, for the message.
type unservableModel struct {
	model *model.Model
	hosts string
}

// unservableModels lists the enabled models whose bindings, of which each has at least one, are all disabled or at hosts requests are not routed to. The policies granting one report it (checkGrantedModels): a model nobody grants is no fault.
func unservableModels(snap *appcatalog.Snapshot, rows []*model.Model) []unservableModel {
	var out []unservableModel
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
		slices.Sort(hosts)
		out = append(out, unservableModel{model: m, hosts: strings.Join(slices.Compact(hosts), ", ")})
	}
	return out
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
