package modelroutes

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
)

// Change kinds Diff reports, one Change per kind per model.
const (
	KindAdded        = "added"   // the catalog now serves a model it did not
	KindRemoved      = "removed" // the catalog no longer serves the model on its route
	KindHost         = "host"    // the default binding moved to another host (routes without a host)
	KindWireName     = "wire-name"
	KindAdapter      = "adapter"
	KindPrice        = "price"
	KindWindow       = "window"
	KindEffort       = "effort" // reasoning-effort levels or their default
	KindCapabilities = "capabilities"
)

// Change is one difference for one listed model. Key is the [models] key, the picker id.
type Change struct{ Key, Kind, Detail string }

// Diff reports how moving f from catalog from to catalog to changes the models f lists, in file order. Models whose route is missing or unusable are skipped; Resolve reports those.
func Diff(f *File, from, to *catalog.IndexedCatalog) []Change {
	routes := map[string]Route{}
	for _, r := range f.Routes {
		routes[r.Name] = r
	}
	var changes []Change
	for _, m := range f.Models {
		r, ok := routes[m.Route]
		if !ok {
			continue
		}
		before, errBefore := resolveEntry(from, m, r)
		after, errAfter := resolveEntry(to, m, r)
		had, has := errBefore == nil && before.Cataloged, errAfter == nil && after.Cataloged
		switch {
		case had && !has:
			changes = append(changes, Change{m.Key, KindRemoved, "not served " + where(r, to)})
		case !had && has:
			changes = append(changes, Change{m.Key, KindAdded, "served " + where(r, to)})
		case had && has:
			changes = append(changes, entryChanges(m.Key, before, after)...)
		}
	}
	return changes
}

func where(r Route, ic *catalog.IndexedCatalog) string {
	if r.Host != "" {
		return fmt.Sprintf("on host %s in catalog %s", r.Host, ic.Catalog.Version)
	}
	return "in catalog " + ic.Catalog.Version
}

func entryChanges(key string, a, b Entry) []Change {
	var out []Change
	add := func(kind string, parts []string) {
		if len(parts) > 0 {
			out = append(out, Change{key, kind, strings.Join(parts, "; ")})
		}
	}
	add(KindHost, valueChange("", a.Host.Name, b.Host.Name))
	add(KindWireName, valueChange("", a.Binding.Name, b.Binding.Name))
	add(KindAdapter, valueChange("", a.Binding.Adapter, b.Binding.Adapter))
	add(KindPrice, priceChanges(a.Binding.Pricing, b.Binding.Pricing))
	var window []string
	window = append(window, valueChange("input", a.Info.ContextWindowInput, b.Info.ContextWindowInput)...)
	window = append(window, valueChange("output", a.Info.ContextWindowOutput, b.Info.ContextWindowOutput)...)
	window = append(window, valueChange("total", a.Info.ContextWindowTotal, b.Info.ContextWindowTotal)...)
	window = append(window, valueChange("maxOutput", a.Info.MaxOutputTokens, b.Info.MaxOutputTokens)...)
	add(KindWindow, window)
	add(KindEffort, effortChanges(a.Info.Capabilities, b.Info.Capabilities))
	add(KindCapabilities, capabilityChanges(a.Info.Capabilities, b.Info.Capabilities))
	return out
}

func effortChanges(a, b catalog.Capabilities) []string {
	var parts []string
	if !slices.Equal(a.ReasoningEfforts, b.ReasoningEfforts) {
		parts = append(parts, fmt.Sprintf("reasoningEfforts %s → %s", levelsText(a.ReasoningEfforts), levelsText(b.ReasoningEfforts)))
	}
	if a.DefaultReasoningEffort != b.DefaultReasoningEffort {
		parts = append(parts, fmt.Sprintf("defaultReasoningEffort %s → %s", orNone(a.DefaultReasoningEffort), orNone(b.DefaultReasoningEffort)))
	}
	return parts
}

func levelsText(levels []string) string {
	if len(levels) == 0 {
		return "none"
	}
	return "[" + strings.Join(levels, " ") + "]"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func valueChange[T comparable](label string, a, b T) []string {
	if a == b {
		return nil
	}
	s := fmt.Sprintf("%v → %v", a, b)
	if label != "" {
		s = label + " " + s
	}
	return []string{s}
}

// capabilityChanges walks every Capabilities field except the effort ones by reflection, so fields added to the catalog later are compared without touching Diff.
func capabilityChanges(a, b catalog.Capabilities) []string {
	a.ReasoningEfforts, a.DefaultReasoningEffort = nil, ""
	b.ReasoningEfforts, b.DefaultReasoningEffort = nil, ""
	var parts []string
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range av.NumField() {
		x, y := av.Field(i).Interface(), bv.Field(i).Interface()
		if reflect.DeepEqual(x, y) {
			continue
		}
		field := av.Type().Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		parts = append(parts, fmt.Sprintf("%s %v → %v", name, x, y))
	}
	return parts
}

// priceChanges matches rates by meter and tier threshold, in the order they first appear.
func priceChanges(a, b []catalog.Rate) []string {
	type tier struct {
		meter string
		above int
	}
	var order []tier
	before, after := map[tier]catalog.Rate{}, map[tier]catalog.Rate{}
	for _, side := range []struct {
		rates []catalog.Rate
		into  map[tier]catalog.Rate
	}{{a, before}, {b, after}} {
		for _, r := range side.rates {
			t := tier{r.Meter, r.AboveTokens}
			if _, seen := before[t]; !seen {
				if _, seen := after[t]; !seen {
					order = append(order, t)
				}
			}
			side.into[t] = r
		}
	}
	var parts []string
	for _, t := range order {
		x, hadX := before[t]
		y, hadY := after[t]
		if hadX == hadY && x == y {
			continue
		}
		label := t.meter
		if t.above > 0 {
			label += " above " + strconv.Itoa(t.above)
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s", label, rateText(x, hadX), rateText(y, hadY)))
	}
	return parts
}

func rateText(r catalog.Rate, ok bool) string {
	if !ok {
		return "none"
	}
	return strconv.FormatFloat(r.Amount, 'g', -1, 64) + " " + r.Unit
}
