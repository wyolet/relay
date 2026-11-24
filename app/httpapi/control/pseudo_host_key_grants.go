// What a pseudo host key asks of the caller before its value is sealed: the grants and owner rules the host key's create, and a new account's, run for the owner, through the same functions.

package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/meta"
)

// mayCreateHostKeyForm refuses a body whose owner the host key's create would refuse, settled as that create settles it, or whose host key or new account the caller may not create under that owner. The rows' own creates ask again in the transaction.
func (d Deps) mayCreateHostKeyForm(ctx context.Context, body pseudo.HostKeyForm) error {
	s, owner := body.Spec, body.Metadata.Owner
	hk := hostKeyChecks(d)
	if err := settleCreateOwner(ctx, hk, meta.OwnerUser, &owner); err != nil {
		return refusal(err, "/metadata/owner")
	}
	if err := hostkey.ValidateOwner(body.Metadata.Name, owner); err != nil {
		return refusal(huma.Error400BadRequest(err.Error()), "/metadata/owner")
	}
	if s.Value != "" || s.ValueFrom != nil {
		if err := hk.mayCreate(ctx, &owner); err != nil {
			return refusal(err, "/metadata/owner")
		}
	}
	if s.Account != nil {
		if err := providerAccountChecks(d).mayCreate(ctx, &owner); err != nil {
			return refusal(err, "/spec/account")
		}
	}
	return nil
}
