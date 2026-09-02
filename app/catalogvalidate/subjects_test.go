package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func serviceAccountDoc(name, projectName, policyName string) manifest.Document {
	return manifest.Document{ServiceAccount: &manifest.ServiceAccountDTO{
		APIVersion: "v1alpha2",
		Kind:       "ServiceAccount",
		Metadata:   manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "project", Name: projectName}},
		Spec:       manifest.ServiceAccountSpec{Project: projectName, Policy: policyName},
	}}
}

func keyDoc(name, principalKind, principalName string) manifest.Document {
	return manifest.Document{Key: &manifest.KeyDTO{
		APIVersion: "v1alpha2",
		Kind:       "Key",
		Metadata:   manifest.WireMeta{Name: name},
		Spec: manifest.KeySpec{
			Principal: manifest.PrincipalDTO{Kind: principalKind, Name: principalName},
		},
	}}
}

func TestValidateGraph_ServiceAccountRefs(t *testing.T) {
	docs := append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"),
		serviceAccountDoc("indexer", "ml-search", ""))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("service account with a present project should be clean, got:\n%s", Format(issues))
	}

	docs = append(fixture(), teamDoc("platform"), serviceAccountDoc("indexer", "ml-search", ""))
	issues := ValidateGraph(docs)
	if !HasErrors(issues) || !strings.Contains(Format(issues), `project "ml-search" not found`) {
		t.Errorf("missing project should error, got:\n%s", Format(issues))
	}

	docs = append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"),
		serviceAccountDoc("indexer", "ml-search", "no-such-policy"))
	issues = ValidateGraph(docs)
	if !HasErrors(issues) || !strings.Contains(Format(issues), `policy "no-such-policy" not found`) {
		t.Errorf("missing policy override should error, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_KeyPrincipalRef(t *testing.T) {
	base := append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"),
		serviceAccountDoc("indexer", "ml-search", ""))

	docs := append(base, keyDoc("indexer-prod", "serviceaccount", "indexer"))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("key naming a present service account should be clean, got:\n%s", Format(issues))
	}

	docs = append(base, keyDoc("ghost", "serviceaccount", "no-such-account"))
	issues := ValidateGraph(docs)
	if !HasErrors(issues) || !strings.Contains(Format(issues), `service account "no-such-account" not found`) {
		t.Errorf("missing service account should error, got:\n%s", Format(issues))
	}

	// Users are not catalog documents, so a user principal is never checked.
	docs = append(base, keyDoc("alice-personal", "user", "alice"))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("user principal should not be checked, got:\n%s", Format(issues))
	}
}
