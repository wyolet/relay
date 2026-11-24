package pseudo

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
)

// AccountRebinding binds an existing account alone to a policy with the write PUT /accounts/{ref}/policy makes: the account leaves the binding holding it, which shrinks or goes, and a binding owned by the account's scope is made. Binding is that binding once written, and what the plan check sees.
type AccountRebinding struct {
	Account  *account.Account
	PolicyID string
	Binding  policybinding.PolicyBinding
}
