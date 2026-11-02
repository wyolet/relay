//go:build integration

package integration_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// sharesCatalog is the host both stacks get before the bundle; export leaves catalog rows out.
const sharesCatalog = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata:
  name: rt-sh-host
spec:
  baseURL: https://rs.invalid
`

// sharesBundle shares one provider account with a team and with a service account, and gives the team a pool drawing on its share. The shares come first, so the plan must order the real account before them.
const sharesBundle = sharesCatalog + `---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Team
metadata:
  name: rt-sh-team
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Project
metadata:
  name: rt-sh-project
spec:
  team: rt-sh-team
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: ServiceAccount
metadata:
  name: rt-sh-sa
spec:
  project: rt-sh-project
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata:
  name: rt-sh-share-team
  owner: {kind: team, name: rt-sh-team}
spec:
  host: rt-sh-host
  sourceAccount: rt-sh-org
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata:
  name: rt-sh-share-sa
  owner: {kind: project, name: rt-sh-project}
spec:
  host: rt-sh-host
  sourceAccount: rt-sh-org
  serviceAccount: rt-sh-sa
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata:
  name: rt-sh-org
  owner: {kind: project, name: rt-sh-project}
spec:
  host: rt-sh-host
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Pool
metadata:
  name: rt-sh-pool
  owner: {kind: team, name: rt-sh-team}
spec:
  host: rt-sh-host
  accounts: [rt-sh-share-team]
`

// Shares and the accounts a pool draws on survive export and apply: the export applies back as a no-op, and a fresh relay given it exports the same documents.
func TestIntegration_SharesRoundTripThroughApply(t *testing.T) {
	t.Parallel()
	first := newStack(t)
	if code, _, raw := first.applyBundle(sharesBundle, ""); code != http.StatusOK {
		t.Fatalf("apply bundle = %d %s", code, raw)
	}
	all, text := exportedSpecs(t, first)
	want := map[string]any{}
	for k, v := range all {
		if strings.Contains(k, "/rt-sh-") {
			want[k] = v
		}
	}
	field := func(doc, name string) any {
		d, ok := want[doc].(map[string]any)
		if !ok {
			t.Fatalf("export lacks %s:\n%s", doc, text)
		}
		return d["spec"].(map[string]any)[name]
	}
	for _, tc := range []struct {
		doc, field string
		want       any
	}{
		{"Account/rt-sh-share-team", "sourceAccount", "rt-sh-org"},
		{"Account/rt-sh-share-sa", "sourceAccount", "rt-sh-org"},
		{"Account/rt-sh-share-sa", "serviceAccount", "rt-sh-sa"},
		{"Pool/rt-sh-pool", "accounts", []any{"rt-sh-share-team"}},
	} {
		if got := field(tc.doc, tc.field); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("exported %s spec.%s = %#v, want %#v", tc.doc, tc.field, got, tc.want)
		}
	}

	_, again, raw := first.applyBundle(text, "dryRun=true")
	for _, e := range again.Plan {
		if e.Action != "unchanged" {
			t.Fatalf("re-applying the export reports %s on %s/%s (%v)\n%s", e.Action, e.Kind, e.Name, e.ChangedFields, raw)
		}
	}

	second := newStack(t)
	if code, _, raw := second.applyBundle(sharesCatalog, ""); code != http.StatusOK {
		t.Fatalf("apply catalog to the second relay = %d %s", code, raw)
	}
	if code, _, raw := second.applyBundle(text, ""); code != http.StatusOK {
		t.Fatalf("apply the export to the second relay = %d %s", code, raw)
	}
	got, _ := exportedSpecs(t, second)
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s after the round trip:\n got  %v\n want %v", k, got[k], w)
		}
	}
}
