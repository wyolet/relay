package v1

import "github.com/wyolet/relay/app/meta"

// ProjectOwner bills a request to the project with this id.
func ProjectOwner(id string) Owner { return Owner{Kind: meta.OwnerProject, ID: id} }
