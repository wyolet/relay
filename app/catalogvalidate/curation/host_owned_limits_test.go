package curation

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/manifest"
)

func TestHostOwnedLimits(t *testing.T) {
	host := manifest.WireOwner{Kind: "host", Name: "h"}
	system := manifest.WireOwner{Kind: "system"}
	docs := []manifest.Document{
		{Policy: &manifest.PolicyDTO{Metadata: manifest.WireMeta{Name: "h-tier-1", Owner: host}}},
		{RateLimit: &manifest.RateLimitDTO{Metadata: manifest.WireMeta{Name: "h-tier-1-rpm", Owner: host}}},
		{Policy: &manifest.PolicyDTO{Metadata: manifest.WireMeta{Name: "sys-policy", Owner: system}}},
		{RateLimit: &manifest.RateLimitDTO{Metadata: manifest.WireMeta{Name: "ownerless-rpm"}}},
	}
	var got []string
	for _, is := range checkHostOwnedLimits(docs) {
		got = append(got, is.Source.Kind+"/"+is.Source.Name+": "+is.Message)
	}
	want := []string{
		"Policy/sys-policy: a catalog Policy must be owned by its host, not the system",
		"RateLimit/ownerless-rpm: a catalog RateLimit must be owned by its host, not the system",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("issues (-want +got):\n%s", diff)
	}
}
