package otlpreceiver

import (
	"strings"

	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/usage"
)

// The error kinds a reported failure is stored under. They are the kinds proxied requests record for a failure of the call to the provider, so one filter or grouping covers both sources. What the client reported is kept in the extras under ExtrasKeyReportedError.
const (
	// errorKindUpstream is a provider that answered with an error status.
	errorKindUpstream = usage.ErrorKindUpstream
	// errorKindUnreachable is a provider the client could not connect to.
	errorKindUnreachable = "upstream_unreachable"
	errorKindTimeout     = "timeout"
	errorKindCanceled    = "client_canceled"
	// errorKindOther is a failure that says no more about itself.
	errorKindOther = "error"
)

// errorKind maps a failed call to one of the kinds above, and a call that did not fail to "". A known provider status decides first: the provider answered, whatever name the client gave the error. Otherwise the reported type is matched by the words every SDK uses for a timeout, a cancellation and a failed connection; exception class names and provider error codes beyond those are not interpreted.
func errorKind(inf otlp.Inference) string {
	if inf.HTTPStatus >= 400 {
		return errorKindUpstream
	}
	if inf.ErrorType == "" {
		return ""
	}
	reported := strings.ToLower(inf.ErrorType)
	switch {
	case containsAny(reported, "timeout", "timed out", "timedout", "deadline"):
		return errorKindTimeout
	case containsAny(reported, "cancel"):
		return errorKindCanceled
	case containsAny(reported, "connect", "econnre", "enotfound", "unreachable"):
		return errorKindUnreachable
	}
	return errorKindOther
}

func containsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
