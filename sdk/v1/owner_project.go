package v1

// ProjectOwner bills a request to the project with this id.
func ProjectOwner(id string) Owner { return Owner{Kind: OwnerProject, ID: id} }
