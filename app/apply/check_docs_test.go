package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

const checkDocsProvider = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: acme
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: acme-1
  owner: {kind: provider, name: acme}
spec:
  snapshots: [{name: acme-1}]
  pointer: `

func TestCheckDocsAcceptsASeedableTree(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(checkDocsProvider + "acme-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDocs(context.Background(), docs); err != nil {
		t.Fatalf("CheckDocs = %v, want nil", err)
	}
}

// A row the graph linter passes but its own Validate refuses fails the seed, so it must fail the check.
func TestCheckDocsRefusesWhatValidateRefuses(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(checkDocsProvider + "acme-2\n"))
	if err != nil {
		t.Fatal(err)
	}
	var invalid *InvalidError
	if err := CheckDocs(context.Background(), docs); !errors.As(err, &invalid) || invalid.Name != "acme-1" {
		t.Fatalf("CheckDocs = %v, want InvalidError for acme-1", err)
	}
}
