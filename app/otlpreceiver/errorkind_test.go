package otlpreceiver

import (
	"testing"

	"github.com/wyolet/relay/pkg/otlp"
)

func TestErrorKind(t *testing.T) {
	for _, tc := range []struct {
		errorType string
		status    int
		want      string
	}{
		{errorType: "", status: 200, want: ""},
		{errorType: "", status: 0, want: ""},

		// The provider answered: the status decides, whatever the error is called.
		{errorType: "429", status: 429, want: "upstream_error"},
		{errorType: "rate_limit_exceeded", status: 429, want: "upstream_error"},
		{errorType: "RateLimitError", status: 429, want: "upstream_error"},
		{errorType: "timeout", status: 504, want: "upstream_error"},
		{errorType: "_OTHER", status: 500, want: "upstream_error"},

		// No status: only the names every SDK shares are read.
		{errorType: "timeout", want: "timeout"},
		{errorType: "APITimeoutError", want: "timeout"},
		{errorType: "<class 'httpx.ReadTimeout'>", want: "timeout"},
		{errorType: "DEADLINE_EXCEEDED", want: "timeout"},
		{errorType: "CancelledError", want: "client_canceled"},
		{errorType: "canceled", want: "client_canceled"},
		{errorType: "APIConnectionError", want: "upstream_unreachable"},
		{errorType: "ECONNREFUSED", want: "upstream_unreachable"},

		// Anything else is a failure of unknown kind, not the client's own word for it.
		{errorType: "<class 'openai.RateLimitError'>", want: "error"},
		{errorType: "RateLimitError", want: "error"},
		{errorType: "rate_limit_exceeded", want: "error"},
		{errorType: "_OTHER", want: "error"},
		{errorType: "java.lang.IllegalStateException", want: "error"},
	} {
		got := errorKind(otlp.Inference{ErrorType: tc.errorType, HTTPStatus: tc.status})
		if got != tc.want {
			t.Errorf("errorKind(%q, status %d) = %q, want %q", tc.errorType, tc.status, got, tc.want)
		}
	}
}
