package embed

import (
	"sort"

	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
)

// Index is a Source over plain kind slices. It keeps the rows relay's
// snapshot build keeps for these kinds, so a tree flattens the same way
// whether relay seeded it or not: a model whose provider is absent, a pricing
// whose host is absent or that targets no kept model, and a binding whose
// model or host is absent all drop out. Disabled rows stay; Flatten skips them.
type Index struct {
	providersByID      map[string]*provider.Provider
	hostsByID          map[string]*host.Host
	modelsByID         map[string]*model.Model
	pricingsByID       map[string]*pricing.Pricing
	pricingByModelHost map[string]*pricing.Pricing
	bindingsByModel    map[string][]*binding.Binding
}

// NewIndex indexes the rows; a later row wins an id or a (model, host)
// pricing slot an earlier one also claims. The rows are read, never copied or
// changed.
func NewIndex(provs []*provider.Provider, hosts []*host.Host, models []*model.Model, pricings []*pricing.Pricing, bindings []*binding.Binding) *Index {
	x := &Index{
		providersByID:      make(map[string]*provider.Provider, len(provs)),
		hostsByID:          make(map[string]*host.Host, len(hosts)),
		modelsByID:         make(map[string]*model.Model, len(models)),
		pricingsByID:       make(map[string]*pricing.Pricing, len(pricings)),
		pricingByModelHost: map[string]*pricing.Pricing{},
		bindingsByModel:    map[string][]*binding.Binding{},
	}
	for _, p := range provs {
		x.providersByID[p.Meta.ID] = p
	}
	for _, h := range hosts {
		x.hostsByID[h.Meta.ID] = h
	}
	for _, m := range models {
		if _, ok := x.providersByID[m.Meta.Owner.ID]; ok {
			x.modelsByID[m.Meta.ID] = m
		}
	}
	for _, p := range pricings {
		hostID := p.Meta.Owner.ID
		if _, ok := x.hostsByID[hostID]; !ok {
			continue
		}
		var targets []string
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := x.modelsByID[modelID]; ok {
				targets = append(targets, modelID)
			}
		}
		if len(targets) == 0 {
			continue
		}
		x.pricingsByID[p.Meta.ID] = p
		for _, modelID := range targets {
			x.pricingByModelHost[modelID+"|"+hostID] = p
		}
	}
	for _, b := range bindings {
		if _, ok := x.modelsByID[b.Spec.ModelID]; !ok {
			continue
		}
		if _, ok := x.hostsByID[b.Spec.HostID]; !ok {
			continue
		}
		x.bindingsByModel[b.Spec.ModelID] = append(x.bindingsByModel[b.Spec.ModelID], b)
	}
	for _, list := range x.bindingsByModel {
		sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	}
	return x
}

// EnabledHosts returns enabled hosts sorted by Meta.Name.
func (x *Index) EnabledHosts() []*host.Host {
	out := make([]*host.Host, 0, len(x.hostsByID))
	for _, h := range x.hostsByID {
		if h.IsEnabled() {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// EnabledModels returns enabled models sorted by Meta.Name.
func (x *Index) EnabledModels() []*model.Model {
	out := make([]*model.Model, 0, len(x.modelsByID))
	for _, m := range x.modelsByID {
		if m.IsEnabled() {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

func (x *Index) Provider(id string) (*provider.Provider, bool) {
	p, ok := x.providersByID[id]
	return p, ok
}

func (x *Index) ProviderSlug(providerID string) (string, bool) {
	p, ok := x.providersByID[providerID]
	if !ok {
		return "", false
	}
	return p.Meta.Name, true
}

// BindingsForModel returns the model's kept bindings sorted by name.
func (x *Index) BindingsForModel(modelID string) []*binding.Binding {
	return x.bindingsByModel[modelID]
}

// PricingForBinding returns the binding's explicit pricing when it was kept,
// else the pricing covering the binding's (model, host) pair.
func (x *Index) PricingForBinding(b *binding.Binding) (*pricing.Pricing, bool) {
	if b == nil {
		return nil, false
	}
	if b.Spec.PricingID != "" {
		if p, ok := x.pricingsByID[b.Spec.PricingID]; ok {
			return p, true
		}
	}
	p, ok := x.pricingByModelHost[b.Spec.ModelID+"|"+b.Spec.HostID]
	return p, ok
}
