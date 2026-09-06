package apply

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
)

// Catalog rows shipped before owners carried an id land with
// owner {kind: user, id: ""}, which names nobody. TestPrunableSkipsOwnerlessUserRows
// pins that prune leaves them alone rather than reading them as a tenant row
// no manifest declares.
func TestPrunableSkipsOwnerlessUserRows(t *testing.T) {
	ownerless := meta.Owner{Kind: meta.OwnerUser}
	for _, kind := range []string{"Policy", "HostKey", "RateLimit", "Key"} {
		if prunable(kind, "row", ownerless) {
			t.Fatalf("prunable(%s, {user, \"\"}) = true, want false", kind)
		}
	}
	if !prunable("Policy", "row", meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}) {
		t.Fatal("a row owned by a real user must stay prunable")
	}
}
