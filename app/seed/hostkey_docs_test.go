package seed

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

const envHostKeyTree = `apiVersion: relay.wyolet.dev/v1alpha2
kind: HostKey
metadata:
  name: tree-key
spec:
  hostId: tree-host
  policyId: tree-tier
  valueFrom:
    kind: env
    env: RELAY_ADMIN_TOKEN
`

// A catalog tree carries templates, not credentials: a HostKey in it would
// resolve a relay environment variable and send it to a host the same tree
// defines. The operator's own seed (`relay seed --apply`) still takes it.
func TestCatalogTreeRefusesHostKeys(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(envHostKeyTree))
	if err != nil {
		t.Fatal(err)
	}
	keep, _, refused := splitSeedDocs(slices.Clone(docs), true)
	if len(keep) != 0 || !slices.Equal(refused, []string{"HostKey/tree-key"}) {
		t.Fatalf("kept %d, refused %v; want the HostKey refused", len(keep), refused)
	}
	keep, _, refused = splitSeedDocs(slices.Clone(docs), false)
	if len(keep) != 1 || len(refused) != 0 {
		t.Fatalf("operator seed: kept %d, refused %v; want the HostKey kept", len(keep), refused)
	}
}
