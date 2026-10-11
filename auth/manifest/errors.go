package manifest

import (
	"errors"
	"fmt"
	"strconv"
)

// Error is a problem with one document. Doc is its index in the stream, Field the path inside it ("spec.rules[0].verbs[1]"), empty when the problem is the document as a whole.
type Error struct {
	Doc  int
	Kind string
	Name string
	// Field is the dotted path to the offending value.
	Field string
	Err   error
}

func (e *Error) Error() string {
	s := "doc " + strconv.Itoa(e.Doc)
	if e.Kind != "" {
		s += " " + e.Kind
	}
	if e.Name != "" {
		s += " " + strconv.Quote(e.Name)
	}
	if e.Field != "" {
		s += ": " + e.Field
	}
	return s + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// RefError is a name a document refers to that resolves to no row. Kind is the referenced kind in the words an error message uses ("team", "service account").
type RefError struct {
	Kind string
	Name string
}

func (e *RefError) Error() string { return fmt.Sprintf("%s %q not found", e.Kind, e.Name) }

// ErrPlainPassword reports a password written in clear text.
var ErrPlainPassword = errors.New("plain-text passwords are refused; set password.hash to a bcrypt hash or password.valueFrom.env to an environment variable")

// ErrOwnerDerived reports a metadata.owner, which these kinds derive rather than take.
var ErrOwnerDerived = errors.New("metadata.owner is not accepted: the owner is derived from the kind and its spec (the system for Team, Role and Group, the parent for Project and ServiceAccount, the scope for RoleBinding)")

// docError wraps err for doc d at field, keeping an *Error err already is.
func docError(d *Document, field string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: field, Err: err}
}

// item is the path of element i of the list at path.
func item(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }
