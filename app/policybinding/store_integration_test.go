//go:build integration

// store_integration_test.go covers the PolicyBinding store against a real Postgres: subjects live in a junction whose FK columns drop a subject when the row it names is deleted. Run with: make test-integration.

package policybinding_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

func TestIntegration_SubjectsOfEveryKindRoundTripAndDropOut(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	q := gen.New(db)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "eval", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	keys := hostkey.NewStore(db, reg, nil)
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
	}
	if err := keys.Upsert(ctx, hk); err != nil {
		t.Fatalf("upsert host key: %v", err)
	}
	acct := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: account.Spec{HostID: hst.Meta.ID},
	}
	if err := account.NewStore(q).Upsert(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "contractors", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	store := policybinding.NewStore(db)
	subjects := []rolebinding.Subject{
		{Kind: policybinding.SubjectHost, ID: hst.Meta.ID},
		{Kind: policybinding.SubjectCredential, ID: hk.Meta.ID},
		{Kind: policybinding.SubjectAccount, ID: acct.Meta.ID},
		{Kind: policybinding.SubjectTeam, ID: tm.Meta.ID},
		{Kind: policybinding.SubjectProject, ID: prj.Meta.ID},
		{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"},
	}
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "contractors-everywhere"},
		Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: subjects},
	}
	if err := store.Upsert(ctx, b); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, b.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if !reflect.DeepEqual(got.Spec.Subjects, subjects) {
		t.Fatalf("subjects = %+v, want %+v", got.Spec.Subjects, subjects)
	}

	// Deleting the credential and the host (which takes its account with it) removes them from the binding; the other subjects stay.
	if err := keys.Delete(ctx, hk.Meta.ID); err != nil {
		t.Fatalf("delete host key: %v", err)
	}
	if err := host.NewStore(q).Delete(ctx, hst.Meta.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	got, err = store.Get(ctx, b.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after deletes: %v (row %v)", err, got)
	}
	if want := subjects[3:]; !reflect.DeepEqual(got.Spec.Subjects, want) {
		t.Fatalf("subjects after deletes = %+v, want %+v", got.Spec.Subjects, want)
	}
}
