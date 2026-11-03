package plan

import "testing"

// A disabled host is not routed to: the route fails over to the other host, and with both disabled the model has no binding to serve it.
func TestRoute_SkipsDisabledHost(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	off := clone(b.hostA)
	off.Spec.Enabled = boolp(false)
	apply(h, off)
	r := h.route(b.key, "m1")
	if r.Refused != nil {
		t.Fatalf("refused with host-b enabled: %v", r.Refused)
	}
	for _, hop := range r.Hops {
		if hop.Host.Meta.ID == b.hostA.Meta.ID {
			t.Fatalf("route reaches disabled host-a: %v", slotIDs(r))
		}
	}

	offB := clone(b.hostB)
	offB.Spec.Enabled = boolp(false)
	apply(h, offB)
	if r := h.route(b.key, "m1"); r.Refused == nil || r.Refused.Code != CodeNoBinding {
		t.Fatalf("both hosts disabled: refused %+v, want %s", r.Refused, CodeNoBinding)
	}
}

func boolp(v bool) *bool { return &v }
