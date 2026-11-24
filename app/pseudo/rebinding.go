package pseudo

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
)

// AccountRebinding binds an existing account alone to a policy with the write PUT /accounts/{ref}/policy makes: the account leaves the binding holding it, which shrinks or goes, and a binding owned by the account's scope is made. Binding is that binding once written; Held is the binding the account was taken out of, as read before, and Left that binding as the write left it, nil when it went. The plan check sees Binding made, Left updated, or Held deleted.
type AccountRebinding struct {
	Account  *account.Account
	PolicyID string
	Binding  policybinding.PolicyBinding
	Held     *policybinding.PolicyBinding
	Left     *policybinding.PolicyBinding
}

// heldRow is the row the rebinding changed besides the binding it made, and how: Left updated, Held deleted, or nothing.
func (rb *AccountRebinding) heldRow() (*policybinding.PolicyBinding, Op) {
	switch {
	case rb.Held == nil:
		return nil, OpCreate
	case rb.Left != nil:
		return rb.Left, OpUpdate
	}
	return rb.Held, OpDelete
}
