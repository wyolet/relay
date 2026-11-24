package adapter

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
)

// Failover hands the same forwarded headers to every attempt; each attempt sets its own credential.
func TestNewRequestCopiesHeadersPerAttempt(t *testing.T) {
	s := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: AuthStrategy{Header: "Authorization", Scheme: "Bearer"}}).Build()
	a := &specAdapter{spec: s}
	hdr := http.Header{"Accept": {"application/json"}, "X-Multi": {"a", "b"}}

	first, release1, err := a.newRequest(context.Background(), "http://first.invalid/", "key-1", []byte(`{}`), hdr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release1()
	second, release2, err := a.newRequest(context.Background(), "http://second.invalid/", "key-2", []byte(`{}`), hdr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()

	first.Header["X-Multi"] = append(first.Header["X-Multi"], "c")
	first.Header.Set("Accept", "text/plain")

	if got := second.Header.Get("Authorization"); got != "Bearer key-2" {
		t.Errorf("second Authorization = %q, want Bearer key-2", got)
	}
	if got := second.Header.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("second X-Multi = %q, want [a b]", got)
	}
	if got := second.Header.Get("Accept"); got != "application/json" {
		t.Errorf("second Accept = %q, want application/json", got)
	}
	if len(hdr) != 2 || hdr.Get("Authorization") != "" || hdr.Get("Content-Type") != "" || len(hdr["X-Multi"]) != 2 || hdr.Get("Accept") != "application/json" {
		t.Errorf("forwarded headers changed: %v", hdr)
	}
}

// A header the envelope joins goes out on one line when the caller sent it on several; a single line and every other header are left as sent, and the forwarded headers are not touched.
func TestNewRequestJoinsEnvelopeHeaders(t *testing.T) {
	s := (&Spec{Name: "shape", Envelope: &RequestEnvelope{JoinHeaders: []string{"x-multi", "X-Single"}}}).Build()
	a := &specAdapter{spec: s}
	hdr := http.Header{"X-Multi": {"a", "b,c"}, "X-Single": {"d"}, "X-Other": {"e", "f"}}

	req, release, err := a.newRequest(context.Background(), "http://up.invalid/", "", []byte(`{}`), hdr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	want := http.Header{"X-Multi": {"a,b,c"}, "X-Single": {"d"}, "X-Other": {"e", "f"}, "Content-Type": {"application/json"}}
	if diff := cmp.Diff(want, req.Header); diff != "" {
		t.Errorf("upstream headers (-want +got):\n%s", diff)
	}
	if len(hdr["X-Multi"]) != 2 {
		t.Errorf("forwarded headers changed: %v", hdr)
	}
}
