package directory

import (
	"errors"
	"fmt"
)

// ErrManaged reports a write to a group that a provisioning source manages, made by someone else without Writer.Force. The error returned is a *ManagedError.
var ErrManaged = errors.New("directory: managed by a provisioning source")

// ManagedError is ErrManaged for one row: its kind and name, and the source that manages it.
type ManagedError struct {
	Kind   string
	Name   string
	Source string
}

func (e *ManagedError) Error() string {
	return fmt.Sprintf("%v: %s %q is managed by %q", ErrManaged, e.Kind, e.Name, e.Source)
}

// Is matches ErrManaged.
func (e *ManagedError) Is(target error) bool { return target == ErrManaged }

// External is the provisioning source, such as a SCIM client, a row came from and that source's id for it. Source scopes ID, so two sources may use the same id; a source may hold a row it sent no id for.
type External struct {
	Source string
	ID     string
}

// IsZero reports whether no source is set.
func (e External) IsZero() bool { return e == External{} }

func (e External) validate(kind, name string) error {
	if e.ID != "" && e.Source == "" {
		return fmt.Errorf("%w: %s %q: an external id needs its source", ErrInvalid, kind, name)
	}
	return nil
}

// Writer is who a write comes from. A group with an external source is managed by it: only that source, or a forced write, changes or deletes it.
type Writer struct {
	// Source is the provisioning source writing; it may write the rows it manages.
	Source string
	// Force writes over a row another source manages, as an operator overriding it does.
	Force bool
}

// WithWriter returns a directory whose writes come from w. The receiver is left as it was.
func (d *Directory) WithWriter(w Writer) *Directory {
	c := *d
	c.writer = w
	return &c
}

// checkManaged refuses a write to a group its source manages unless the writer is that source or forced.
func (d *Directory) checkManaged(g *Group) error {
	src := g.Spec.External.Source
	if src == "" || d.writer.Force || d.writer.Source == src {
		return nil
	}
	return &ManagedError{Kind: "group", Name: g.Meta.Name, Source: src}
}
