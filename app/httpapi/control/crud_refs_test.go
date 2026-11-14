package control

import (
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
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
