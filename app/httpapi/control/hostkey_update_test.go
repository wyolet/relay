package control

import (
	"testing"

	"github.com/wyolet/relay/app/hostkey"
)

// A PUT without a value edits the row and keeps the stored secret; only an
// explicit value replaces it.
func TestHostKeyUpdateWithoutValueKeepsTheStoredSecret(t *testing.T) {
	for _, kind := range []hostkey.ValueKind{hostkey.ValueKindStored, hostkey.ValueKindOAuth} {
		t.Run(string(kind), func(t *testing.T) {
			existing := &hostkey.HostKey{Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: kind, Provider: "acme"}}, Resolved: "sk-stored"}
			incoming := &hostkey.HostKey{Spec: existing.Spec}
			mergeHostKeyPreserveValue(existing, incoming)
			if incoming.Spec.Value != "sk-stored" && incoming.Resolved != "sk-stored" {
				t.Fatalf("value=%q resolved=%q, want the stored secret carried", incoming.Spec.Value, incoming.Resolved)
			}

			replaced := &hostkey.HostKey{Spec: existing.Spec}
			replaced.Spec.Value = "sk-new"
			mergeHostKeyPreserveValue(existing, replaced)
			if replaced.Spec.Value != "sk-new" {
				t.Fatalf("an explicit value was overwritten with %q", replaced.Spec.Value)
			}
		})
	}
}
