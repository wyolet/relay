package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

// failOn returns a stream transform that passes frames through unchanged and fails, like a translator decoding a malformed payload, on any frame carrying marker.
func failOn(marker string) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) {
		if bytes.Contains(b, []byte(marker)) {
			var v struct{}
			return nil, json.Unmarshal([]byte(`{`), &v)
		}
		return b, nil
	}
}

func identityStream(b []byte) ([]byte, error) { return b, nil }

func runStreamCanonical(t *testing.T, stream string, shapes streamShapes, toCanon, fromCanon func([]byte) ([]byte, error)) string {
	t.Helper()
	w := httptest.NewRecorder()
	streamCanonical(Deps{}, w, httptest.NewRequest(http.MethodPost, "/v1/generate", nil), io.NopCloser(strings.NewReader(stream)), false, false, shapes, toCanon, fromCanon)
	return w.Body.String()
}

// A frame either translator rejects is skipped and counted against that translator; the rest of the stream reaches the caller untouched. Only when the terminal event is among the lost does relay end the stream itself, with an error the caller cannot read as a completed response.
func TestStreamCanonical_DroppedFrames(t *testing.T) {
	const (
		created   = "event: generation.created\ndata: {\"id\":\"g1\",\"model\":\"m\"}\n\n"
		hel       = "event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"Hel\"}\n\n"
		bad       = "event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"BAD\"}\n\n"
		lo        = "event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"lo\"}\n\n"
		completed = "event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n"
		badEnd    = "event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"BAD\"}\n\n"
		upErr     = "event: error\ndata: {\"code\":\"upstream\",\"message\":\"overloaded\"}\n\n"
		closing   = "event: error\ndata: {\"code\":\"translate_stream\",\"message\":\"relay could not translate part of the upstream stream; the response is incomplete\"}\n\n"
	)
	tests := []struct {
		name      string
		stream    string
		toCanon   func([]byte) ([]byte, error)
		fromCanon func([]byte) ([]byte, error)
		want      string
		// wantDrops is the count per (adapter, direction, reason) label set.
		wantDrops map[[3]string]float64
	}{
		{"nothing dropped", created + hel + lo + completed, identityStream, identityStream, created + hel + lo + completed, nil},
		{"upstream frame", created + hel + bad + lo + completed, failOn("BAD"), identityStream, created + hel + lo + completed,
			map[[3]string]float64{{"up", "to_canonical", "syntax"}: 1}},
		{"inbound frame", created + hel + bad + lo + completed, identityStream, failOn("BAD"), created + hel + lo + completed,
			map[[3]string]float64{{"in", "from_canonical", "syntax"}: 1}},
		{"several frames", created + bad + hel + bad + completed, failOn("BAD"), identityStream, created + hel + completed,
			map[[3]string]float64{{"up", "to_canonical", "syntax"}: 2}},
		{"terminal frame", created + hel + badEnd, failOn("BAD"), identityStream, created + hel + closing,
			map[[3]string]float64{{"up", "to_canonical", "syntax"}: 1}},
		{"terminal frame on the inbound side", created + hel + badEnd, identityStream, failOn("BAD"), created + hel + closing,
			map[[3]string]float64{{"in", "from_canonical", "syntax"}: 1}},
		// An upstream error event already ends the stream; a second error would misreport it.
		{"frame before an upstream error", created + bad + upErr, failOn("BAD"), identityStream, created + upErr,
			map[[3]string]float64{{"up", "to_canonical", "syntax"}: 1}},
		// A stream cut short with nothing dropped is the upstream's truncation, passed on as it came.
		{"no terminal frame and nothing dropped", created + hel, identityStream, identityStream, created + hel, nil},
		{"every frame", bad + bad, failOn("BAD"), identityStream, closing,
			map[[3]string]float64{{"up", "to_canonical", "syntax"}: 2}},
		{"a failure that is not a decode error", created + bad + completed, func(b []byte) ([]byte, error) {
			if bytes.Contains(b, []byte("BAD")) {
				return nil, errors.New("unknown event")
			}
			return b, nil
		}, identityStream, created + completed, map[[3]string]float64{{"up", "to_canonical", "invalid"}: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := [][3]string{{"up", "to_canonical", "syntax"}, {"up", "to_canonical", "invalid"}, {"in", "from_canonical", "syntax"}}
			count := func(l [3]string) float64 {
				return testutil.ToFloat64(metrics.StreamEventsDropped.WithLabelValues(l[0], l[1], l[2]))
			}
			before := map[[3]string]float64{}
			for _, l := range labels {
				before[l] = count(l)
			}

			got := runStreamCanonical(t, tt.stream, streamShapes{upstream: "up", inbound: "in"}, tt.toCanon, tt.fromCanon)

			if got != tt.want {
				t.Errorf("stream\n got %q\nwant %q", got, tt.want)
			}
			for _, l := range labels {
				if d := count(l) - before[l]; d != tt.wantDrops[l] {
					t.Errorf("dropped%v rose by %v, want %v", l, d, tt.wantDrops[l])
				}
			}
		})
	}
}

func TestStreamDropReason(t *testing.T) {
	var n int
	for want, err := range map[string]error{
		"syntax":  json.Unmarshal([]byte(`{`), &n),
		"type":    json.Unmarshal([]byte(`"x"`), &n),
		"invalid": errors.New("unknown event"),
	} {
		if got := streamDropReason(err); got != want {
			t.Errorf("streamDropReason(%v) = %q, want %q", err, got, want)
		}
	}
}
