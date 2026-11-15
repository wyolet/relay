package v1

import (
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
)

// passTranslator declares a to-canonical stream that returns each chunk as is, so the translated branch runs over an already-canonical stream.
type passTranslator struct{ IdentityTranslator }

func (passTranslator) NewToCanonicalStream() func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) { return b, nil }
}

// decoderTranslator answers DecodeSummary with sum, or err.
type decoderTranslator struct {
	IdentityTranslator
	sum Summary
	err error
}

func (d decoderTranslator) DecodeSummary([]byte) (Summary, error) { return d.sum, d.err }

// filterTranslator declares a summary stream that only admits generation.completed frames and records what reaches the translator.
type filterTranslator struct {
	IdentityTranslator
	seen *[]string
}

func (f filterTranslator) NewSummaryStream() (func([]byte) ([]byte, error), func([]byte) bool) {
	return func(b []byte) ([]byte, error) {
			*f.seen = append(*f.seen, string(b))
			return b, nil
		}, func(frame []byte) bool {
			return bytes.Contains(frame, []byte(EventGenerationCompleted))
		}
}

// failingTranslator's stream errors on every chunk.
type failingTranslator struct{ IdentityTranslator }

func (failingTranslator) NewToCanonicalStream() func([]byte) ([]byte, error) {
	return func([]byte) ([]byte, error) { return nil, errors.New("bad chunk") }
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

const (
	createdFrame   = "event: generation.created\ndata: {\"id\":\"g\",\"model\":\"m\"}\n\n"
	deltaFrame     = "event: item.delta\ndata: {\"item_id\":\"i1\",\"index\":0,\"kind\":\"text\",\"delta\":\"x\"}\n\n"
	completedFrame = "event: generation.completed\ndata: {\"id\":\"g\",\"status\":\"completed\",\"finish_reason\":\"stop\",\"usage\":{\"input\":100,\"output\":50,\"cache_read\":25},\"service_tier\":\"priority\"}\n\n"
)

var completedSummary = Summary{Tokens: usage.Tokens{"input": 100, "output": 50, "cache_read": 25}, FinishReason: FinishReasonStop, ServiceTier: "priority"}

func TestExtractSummary(t *testing.T) {
	syncBody := `{"id":"r","usage":{"input":12,"output":34},"finish_reason":"length","service_tier":"flex"}`
	syncSummary := Summary{Tokens: usage.Tokens{"input": 12, "output": 34}, FinishReason: FinishReasonLength, ServiceTier: "flex"}
	stream := createdFrame + deltaFrame + completedFrame
	decoded := Summary{FinishReason: FinishReasonToolCalls, ServiceTier: "flex"}

	tests := []struct {
		name string
		tr   Translator
		body string
		want Summary
	}{
		{"nil translator", nil, syncBody, Summary{}},
		{"empty body", IdentityTranslator{}, "", Summary{}},
		{"sync JSON", IdentityTranslator{}, syncBody, syncSummary},
		{"sync JSON gzipped", IdentityTranslator{}, gzipped(t, syncBody), syncSummary},
		{"sync JSON without usage", IdentityTranslator{}, `{"id":"r"}`, Summary{}},
		{"sync body that does not parse", IdentityTranslator{}, `<html>502</html>`, Summary{}},
		{"sync body through the shape's decoder", decoderTranslator{sum: decoded}, syncBody, decoded},
		{"decoder error is a body without a summary", decoderTranslator{sum: decoded, err: errors.New("bad")}, syncBody, Summary{}},
		{"stream ignores the decoder", decoderTranslator{sum: decoded}, stream, completedSummary},
		{"canonical stream", IdentityTranslator{}, stream, completedSummary},
		{"canonical stream gzipped", IdentityTranslator{}, gzipped(t, stream), completedSummary},
		{"canonical stream with CRLF line ends", IdentityTranslator{}, strings.ReplaceAll(stream, "\n", "\r\n"), completedSummary},
		{"canonical stream after leading blank lines and a BOM", IdentityTranslator{}, "\xef\xbb\xbf\n\n" + stream, completedSummary},
		{"canonical stream without a terminating blank line", IdentityTranslator{}, strings.TrimSuffix(stream, "\n\n"), completedSummary},
		{"stream without generation.completed", IdentityTranslator{}, createdFrame + deltaFrame, Summary{}},
		{"generation.completed with malformed data is skipped", IdentityTranslator{}, createdFrame + "event: generation.completed\ndata: {\"usage\":\n\n", Summary{}},
		{"usage under another event name is not harvested", IdentityTranslator{}, "event: item.completed\ndata: {\"usage\":{\"input\":1},\"finish_reason\":\"stop\"}\n\n", Summary{}},
		{"translated stream", passTranslator{}, stream, completedSummary},
		{"translated stream with CRLF line ends", passTranslator{}, strings.ReplaceAll(stream, "\n", "\r\n"), completedSummary},
		{"translator error on every chunk", failingTranslator{}, stream, Summary{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractSummary(tc.tr, []byte(tc.body))
			if err != nil {
				t.Fatalf("ExtractSummary: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("summary (-want +got):\n%s", diff)
			}
			tokens, err := ExtractUsage(tc.tr, []byte(tc.body))
			if diff := cmp.Diff(tc.want.Tokens, tokens); diff != "" || err != nil {
				t.Errorf("ExtractUsage err %v (-want +got):\n%s", err, diff)
			}
		})
	}
}

func TestExtractSummary_CorruptGzip(t *testing.T) {
	body := []byte(gzipped(t, `{"usage":{"input":1}}`))
	for name, corrupt := range map[string][]byte{
		"bad header":     {0x1f, 0x8b, 0xff, 0xff},
		"truncated body": body[:len(body)-6],
	} {
		got, err := ExtractSummary(IdentityTranslator{}, corrupt)
		if err == nil || !got.isEmpty() {
			t.Errorf("%s: summary %+v, err %v; want an empty summary and an error", name, got, err)
		}
	}
}

// Within one canonical body a later generation.completed fills in what it carries and keeps the rest.
func TestExtractSummary_MergesCompletedFrames(t *testing.T) {
	body := "event: generation.completed\ndata: {\"usage\":{\"input\":1},\"finish_reason\":\"length\",\"service_tier\":\"flex\"}\n\n" +
		"event: generation.completed\ndata: {\"finish_reason\":\"stop\"}\n\n"
	got, err := ExtractSummary(IdentityTranslator{}, []byte(body))
	want := Summary{Tokens: usage.Tokens{"input": 1}, FinishReason: FinishReasonStop, ServiceTier: "flex"}
	if diff := cmp.Diff(want, got); diff != "" || err != nil {
		t.Errorf("err %v (-want +got):\n%s", err, diff)
	}
}

// Only frames the shape says can change the summary reach its translator, each normalized to LF and ended by a blank line.
func TestExtractSummary_SummaryStreamFilter(t *testing.T) {
	var seen []string
	got, err := ExtractSummary(filterTranslator{seen: &seen}, []byte(strings.ReplaceAll(createdFrame+deltaFrame+completedFrame, "\n", "\r\n")))
	if diff := cmp.Diff(completedSummary, got); diff != "" || err != nil {
		t.Errorf("err %v (-want +got):\n%s", err, diff)
	}
	if diff := cmp.Diff([]string{completedFrame}, seen); diff != "" {
		t.Errorf("chunks translated (-want +got):\n%s", diff)
	}
}

// A frame of any size must not stop the walk before the usage frame after it.
func TestExtractSummary_LargeFrameBeforeUsage(t *testing.T) {
	big := "event: item.delta\ndata: {\"item_id\":\"i1\",\"index\":0,\"kind\":\"text\",\"delta\":\"" + strings.Repeat("x", 5<<20) + "\"}\n\n"
	for _, tr := range []Translator{IdentityTranslator{}, passTranslator{}} {
		got, err := ExtractSummary(tr, []byte(big+completedFrame))
		if diff := cmp.Diff(completedSummary, got); diff != "" || err != nil {
			t.Errorf("%T: err %v (-want +got):\n%s", tr, err, diff)
		}
	}
}

func TestExtractSummary_StreamOpeningWithAComment(t *testing.T) {
	got, err := ExtractSummary(IdentityTranslator{}, []byte(": keepalive\n\n"+completedFrame))
	if diff := cmp.Diff(completedSummary, got); diff != "" || err != nil {
		t.Errorf("err %v (-want +got):\n%s", err, diff)
	}
}

func TestLooksLikeSSE(t *testing.T) {
	cases := map[string]bool{
		"event: foo\ndata: {}":               true,
		"data: {}":                           true,
		"data:{}":                            true,
		" \t\r\n\xef\xbb\xbfevent:x":         true,
		strings.Repeat(" ", 251) + "data: x": true,
		strings.Repeat(" ", 252) + "data: x": false,
		`{"foo":1}`:                          false,
		`{"data":"data: x"}`:                 false,
		"":                                   false,
		"dat":                                false,
		"Event: x":                           false,
		strings.Repeat("x", 300) + "\ndata: late":  false,
		": keepalive\n\ndata: x":                   true,
		": OPENROUTER PROCESSING\r\n: x\r\nevent:": true,
		strings.Repeat(": k\n", 100) + "data: x":   true,
		": keepalive\n\n{\"id\":\"r\"}":            false,
		": no line end":                            false,
	}
	for body, want := range cases {
		if got := looksLikeSSE([]byte(body)); got != want {
			t.Errorf("looksLikeSSE(%.40q) = %v, want %v", body, got, want)
		}
	}
}
