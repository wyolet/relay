package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// Every error huma builds leaves as the relay envelope: the type and code follow the status, a 429 and an oversized body get their own message, and the per-field errors of a 422 survive so it names the field that failed.
func TestInstalledErrorEnvelope(t *testing.T) {
	Install()
	field := &huma.ErrorDetail{Message: "expected string", Location: "body.project", Value: 5.0}
	for _, tc := range []struct {
		name   string
		status int
		msg    string
		errs   []error
		want   APIError
	}{
		{"bad request", 400, "no", nil, APIError{Err: APIErrorBody{Type: "invalid_request_error", Message: "no"}, HTTPStatus: 400}},
		{"not found carries no code", 404, "gone", nil, APIError{Err: APIErrorBody{Type: "invalid_request_error", Message: "gone"}, HTTPStatus: 404}},
		{"validation keeps the field errors", 422, "validation failed", []error{field, nil, errors.New("plain")},
			APIError{Err: APIErrorBody{Type: "invalid_request_error", Code: "unprocessable_entity", Message: "validation failed",
				Details: []*huma.ErrorDetail{field, {Message: "plain"}}}, HTTPStatus: 422}},
		{"rate limit replaces the message", 429, "too many token mints; retry shortly", nil,
			APIError{Err: APIErrorBody{Type: "rate_limit_exceeded", Code: "rate_limit_exceeded", Message: "rate limit exceeded"}, HTTPStatus: 429}},
		{"server error", 500, "pg down", nil, APIError{Err: APIErrorBody{Type: "server_error", Code: "internal_error", Message: "pg down"}, HTTPStatus: 500}},
		{"unavailable carries no code", 503, "later", nil, APIError{Err: APIErrorBody{Type: "invalid_request_error", Message: "later"}, HTTPStatus: 503}},
		{"an oversized body is a 413 whatever huma called it", 400, "read failed", []error{fmt.Errorf("read: %w", &http.MaxBytesError{Limit: 1})},
			APIError{Err: APIErrorBody{Type: "invalid_request_error", Code: "request_too_large", Message: "request body too large",
				Details: []*huma.ErrorDetail{{Message: "read: http: request body too large"}}}, HTTPStatus: 413}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := huma.NewError(tc.status, tc.msg, tc.errs...)
			if diff := cmp.Diff(&tc.want, got); diff != "" {
				t.Fatalf("error (-want +got):\n%s", diff)
			}
			if got.GetStatus() != tc.want.HTTPStatus || got.Error() != tc.want.Err.Message {
				t.Fatalf("status %d error %q, want %d %q", got.GetStatus(), got.Error(), tc.want.HTTPStatus, tc.want.Err.Message)
			}
		})
	}

	raw, err := json.Marshal(huma.NewError(422, "validation failed", field))
	const want = `{"error":{"type":"invalid_request_error","code":"unprocessable_entity","message":"validation failed","details":[{"message":"expected string","location":"body.project","value":5}]}}`
	if err != nil || string(raw) != want {
		t.Fatalf("wire form = %s (%v), want %s", raw, err, want)
	}
	if ct := (&APIError{}).ContentType("application/problem+json"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
}

// Declared with the names of control's wrappers: the namer matches a generic by its base name.
type listBody[T any] struct {
	Items []*T `json:"items"`
}

type sectionEnvelope[T any] struct {
	Section settings.SectionName `json:"section"`
	Value   T                    `json:"value"`
}

type unknownWrapper[T any] struct{ Value T }

// Schema ids are clean identifiers: an entity keeps its name, a sub-type of an entity package takes the entity as a prefix so the Spec types do not collide, and a known generic wrapper becomes <Entity><Suffix>.
func TestSchemaNamer(t *testing.T) {
	for _, tc := range []struct {
		in   any
		hint string
		want string
	}{
		{policy.Policy{}, "", "Policy"},
		{policy.Spec{}, "", "PolicySpec"},
		{ratelimit.RateLimit{}, "", "RateLimit"},
		{ratelimit.Spec{}, "", "RateLimitSpec"},
		{ratelimit.Rule{}, "", "RateLimitRule"},
		{pricing.Spec{}, "", "PricingSpec"},
		{pricing.Rate{}, "", "PricingRate"},
		{hostkey.Spec{}, "", "HostKeySpec"},
		{meta.Metadata{}, "", "Metadata"},
		{meta.Owner{}, "", "Owner"},
		{&policy.Spec{}, "", "PolicySpec"},
		{[]*policy.Policy{}, "", "Policy"},
		{[2]pricing.Rate{}, "", "PricingRate"},
		{listBody[policy.Policy]{}, "", "PolicyList"},
		{listBody[pricing.Pricing]{}, "", "PricingList"},
		{sectionEnvelope[settings.ProxyMode]{}, "", "ProxyModeEnvelope"},
		{sectionEnvelope[*settings.ProxyMode]{}, "", "ProxyModeEnvelope"},
		// A wrapper the namer does not know, or one over nothing it can name, keeps its type name with the punctuation flattened.
		{unknownWrapper[policy.Policy]{}, "", "unknownWrapper_github_com_wyolet_relay_pkg_catalogkind_policy_Policy_"},
		{listBody[string]{}, "", "listBody_string_"},
		{struct{ A int }{}, "InlineBody", "InlineBody"},
	} {
		if got := schemaNamer(reflect.TypeOf(tc.in), tc.hint); got != tc.want {
			t.Errorf("schemaNamer(%T) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The UI posts a stored-mode key through this property: the write-only cleartext must stay in the schema.
func TestHostKeySpecSchemaHasValue(t *testing.T) {
	reg := NewRegistry()
	reg.Schema(reflect.TypeOf(hostkey.Spec{}), false, "")
	spec := reg.Map()["HostKeySpec"]
	if spec == nil || spec.Properties["value"] == nil || spec.Properties["value"].Type != "string" {
		t.Fatalf("HostKeySpec = %+v, want a string property value", spec)
	}
}
