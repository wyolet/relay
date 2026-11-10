package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
)

// A key naming an account held at another host is refused in every load,
// the boot seed included, with the message the API's guard uses.
func TestApplyRejectsAHostKeyOnAnotherHostsAccount(t *testing.T) {
	h1 := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "openai"}}
	h2 := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "anthropic"}}
	acct := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: h1.Meta.ID}}
	rows := &Rows{Hosts: []*host.Host{h1, h2}, Accounts: []*account.Account{acct}}
	check := (&builder{rows: rows}).refs().HostKey

	ok := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h1.Meta.ID, AccountID: acct.Meta.ID}}
	if err := check(context.Background(), ok); err != nil {
		t.Fatalf("account at the key's own host: %v", err)
	}
	other := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h2.Meta.ID, AccountID: acct.Meta.ID}}
	if err := check(context.Background(), other); err == nil || !strings.Contains(err.Error(), "held at another host") {
		t.Fatalf("account at another host: err = %v", err)
	}
	unknown := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h1.Meta.ID, AccountID: meta.NewID()}}
	if err := check(context.Background(), unknown); err == nil {
		t.Error("unresolvable account accepted")
	}
}
