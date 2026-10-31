package control

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/refcheck"
)

// A refusal that carries a code answers in the error envelope with that code,
// as a refused delete of a referenced row does.
func TestRefErrKeepsTheRefusalCode(t *testing.T) {
	err := refErr(&refcheck.Error{Status: http.StatusConflict, Code: refcheck.CodeInUse, Msg: "in use"})
	var ae *httpapi.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T, want *httpapi.APIError", err)
	}
	if ae.HTTPStatus != http.StatusConflict || ae.Err.Code != "resource_in_use" || ae.Err.Message != "in use" {
		t.Fatalf("got %d %+v", ae.HTTPStatus, ae.Err)
	}
	if err := mapGuardErr(err); err != error(ae) {
		t.Fatalf("guard mapping replaced the refusal: %v", err)
	}
}

// A delete the store refuses because a row linked the target after the references scan is the caller's conflict, not a missing row.
func TestRestrictedDeleteIsAConflict(t *testing.T) {
	err := restrictedDelete("rate-limit", "rl", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503"}))
	var ae *httpapi.APIError
	if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusConflict || ae.Err.Code != "resource_in_use" {
		t.Fatalf("err = %v, want a 409 resource_in_use", err)
	}
	if restrictedDelete("rate-limit", "rl", errors.New("boom")) != nil {
		t.Fatal("an unrelated store error read as in use")
	}
}

// A binding write the store refuses because another binding holds the subject is the caller's conflict, as the save-time check answers it.
func TestWriteOfAnAlreadyBoundSubjectIsAConflict(t *testing.T) {
	err := mapWriteErr("policy-binding", "pb-1", fmt.Errorf("%w: user:u-1", policybinding.ErrSubjectBound))
	var ae *httpapi.APIError
	if !errors.As(err, &ae) || ae.HTTPStatus != http.StatusConflict || ae.Err.Code != refcheck.CodeDuplicateBinding {
		t.Fatalf("err = %v, want a 409 duplicate_binding", err)
	}
}
