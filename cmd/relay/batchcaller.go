package main

import (
	"context"

	"github.com/wyolet/relay/app/batch"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
)

// batchCaller projects the Principal the inference auth chain resolved onto
// the identity app/batch consumes. Bearer classification and the
// Key → ServiceAccount → PolicyBinding resolution order belong to the HTTP
// layer; the batch subsystem takes the answer, which is why a token
// authenticates a submission exactly as a key does.
func batchCaller(ctx context.Context) *batch.Caller {
	p := inference.PrincipalFrom(ctx)
	if p == nil {
		return nil
	}
	c := &batch.Caller{KeyHash: p.KeyHash, PolicyID: p.PolicyID()}
	c.ProjectID, c.TeamID = p.ProjectID, p.TeamID
	c.CredentialKind, c.CredentialID = p.CredentialKind, p.CredentialID
	switch {
	case p.ServiceAccountID != "":
		c.PrincipalKind, c.PrincipalID = string(key.PrincipalServiceAccount), p.ServiceAccountID
	case p.UserID != "":
		c.PrincipalKind, c.PrincipalID = string(key.PrincipalUser), p.UserID
	default:
		return nil
	}
	return c
}
