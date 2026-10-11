package scim

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

// The scimType values of RFC 7644 section 3.12 the handler answers with.
const (
	typeInvalidFilter = "invalidFilter"
	typeTooMany       = "tooMany"
	typeUniqueness    = "uniqueness"
	typeMutability    = "mutability"
	typeInvalidSyntax = "invalidSyntax"
	typeInvalidPath   = "invalidPath"
	typeNoTarget      = "noTarget"
	typeInvalidValue  = "invalidValue"
)

// Error is a SCIM error response.
type Error struct {
	Status int
	// Type is the scimType, empty for errors RFC 7644 gives none.
	Type   string
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("scim %d %s: %s", e.Status, e.Type, e.Detail) }

func errorf(status int, scimType, format string, args ...any) *Error {
	return &Error{Status: status, Type: scimType, Detail: fmt.Sprintf(format, args...)}
}

func badRequest(scimType, format string, args ...any) *Error {
	return errorf(http.StatusBadRequest, scimType, format, args...)
}

type errorBody struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// writeError answers err as a SCIM error. A directory error is mapped to its status; anything else is a 500 whose detail stays in the server's hands.
func writeError(w http.ResponseWriter, err error) {
	e := toError(err)
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
	}
	writeJSON(w, e.Status, errorBody{
		Schemas: []string{schemaError}, Status: strconv.Itoa(e.Status), ScimType: e.Type, Detail: e.Detail,
	})
}

func toError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return errorf(http.StatusNotFound, "", "resource not found")
	case errors.Is(err, directory.ErrConflict):
		return errorf(http.StatusConflict, typeUniqueness, "%s", err.Error())
	case errors.Is(err, directory.ErrInvalid):
		return badRequest(typeInvalidValue, "%s", err.Error())
	case errors.Is(err, directory.ErrStaleResourceVersion):
		return errorf(http.StatusConflict, "", "the resource changed while the request ran; retry it")
	case errors.Is(err, directory.ErrManaged), errors.Is(err, authz.ErrForbidden):
		return errorf(http.StatusForbidden, "", "forbidden")
	default:
		return errorf(http.StatusInternalServerError, "", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
