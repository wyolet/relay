package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUnmarshalPart(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want Part
	}{
		{"input_text", `{"type":"input_text","text":"hello"}`, &TextPart{Text: "hello"}},
		{"input_text empty keeps the text member", `{"type":"input_text","text":""}`, &TextPart{}},
		{"input_image", `{"type":"input_image","image_url":"https://example.com/img.png","detail":"high"}`,
			&ImagePart{ImageURL: "https://example.com/img.png", Detail: "high"}},
		{"input_image empty", `{"type":"input_image"}`, &ImagePart{}},
		{"input_file by url", `{"type":"input_file","file_url":"https://example.com/doc.pdf","filename":"doc.pdf","media_type":"application/pdf"}`,
			&FilePart{FileURL: "https://example.com/doc.pdf", Filename: "doc.pdf", MediaType: "application/pdf"}},
		{"input_file by id and data", `{"type":"input_file","file_id":"file_1","file_data":"aGk="}`, &FilePart{FileID: "file_1", FileData: "aGk="}},
		{"output_text", `{"type":"output_text","text":"the answer is 42"}`, &OutputTextPart{Text: "the answer is 42"}},
		{
			"url_citation with provider data",
			`{"type":"output_text","text":"x","annotations":[{"type":"url_citation","start_index":0,"end_index":0,"url":"https://x","provider_data":{"type":"web_search_result_location","encrypted_index":"E"}}]}`,
			&OutputTextPart{Text: "x", Annotations: []Annotation{
				&URLCitationAnnotation{URL: "https://x", ProviderData: json.RawMessage(`{"type":"web_search_result_location","encrypted_index":"E"}`)},
			}},
		},
		{
			"output_text with annotations of each kind",
			`{"type":"output_text","text":"see source","annotations":[` +
				`{"type":"url_citation","start_index":4,"end_index":10,"url":"https://example.com","title":"Example"},` +
				`{"type":"text_citation","start_index":0,"end_index":3},` +
				`{"type":"custom_citation","extra":"data"}]}`,
			&OutputTextPart{Text: "see source", Annotations: []Annotation{
				&URLCitationAnnotation{StartIndex: 4, EndIndex: 10, URL: "https://example.com", Title: "Example"},
				&TextCitationAnnotation{StartIndex: 0, EndIndex: 3},
				&RawAnnotation{Type: "custom_citation", JSON: json.RawMessage(`{"type":"custom_citation","extra":"data"}`)},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unmarshalPart([]byte(tc.wire))
			if err != nil {
				t.Fatalf("unmarshalPart: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("part (-want +got):\n%s", diff)
			}
			if got.PartType() != tc.want.PartType() {
				t.Errorf("PartType = %q, want %q", got.PartType(), tc.want.PartType())
			}
			wantJSON(t, got, tc.wire)
		})
	}
}

func TestUnmarshalPart_Errors(t *testing.T) {
	tests := []struct{ name, wire, want string }{
		{"not an object", `"x"`, "part: json: cannot unmarshal string…"},
		{"missing type", `{"text":"x"}`, `unsupported part type ""`},
		{"refusal is not a part", `{"type":"refusal","refusal":"no"}`, `unsupported part type "refusal"`},
		{"input_text with numeric text", `{"type":"input_text","text":1}`, "input_text part: json: cannot unmarshal number…"},
		{"input_image with numeric url", `{"type":"input_image","image_url":1}`, "input_image part: json: cannot unmarshal number…"},
		{"input_file with numeric id", `{"type":"input_file","file_id":1}`, "input_file part: json: cannot unmarshal number…"},
		{"output_text with numeric text", `{"type":"output_text","text":1}`, "output_text part: json: cannot unmarshal number…"},
		{"output_text with a non-object annotation", `{"type":"output_text","annotations":[1]}`, "output_text part: annotation: json: cannot unmarshal number…"},
		{"output_text with a malformed url_citation", `{"type":"output_text","annotations":[{"type":"url_citation","url":1}]}`, "output_text part: url_citation annotation: json: cannot unmarshal number…"},
		{"output_text with a malformed text_citation", `{"type":"output_text","annotations":[{"type":"text_citation","start_index":"a"}]}`, "output_text part: text_citation annotation: json: cannot unmarshal string…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unmarshalPart([]byte(tc.wire))
			if err == nil {
				t.Fatalf("unmarshalPart = %#v, want an error", got)
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestAnnotation_Type(t *testing.T) {
	for want, a := range map[string]Annotation{
		"url_citation":  &URLCitationAnnotation{},
		"text_citation": &TextCitationAnnotation{},
		"custom":        &RawAnnotation{Type: "custom"},
	} {
		if got := a.AnnotationType(); got != want {
			t.Errorf("%T.AnnotationType() = %q, want %q", a, got, want)
		}
	}
	// A RawAnnotation built without its source bytes still marshals its type.
	wantJSON(t, &RawAnnotation{Type: "custom"}, `{"type":"custom"}`)
}

func TestUnmarshalContent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []Part
		err  string
	}{
		{name: "absent", raw: ``, want: nil},
		{name: "string", raw: `"hello"`, want: []Part{&TextPart{Text: "hello"}}},
		{name: "empty string is still one part", raw: `""`, want: []Part{&TextPart{}}},
		{name: "null", raw: `null`, want: []Part{}},
		{name: "array", raw: `[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]`,
			want: []Part{&TextPart{Text: "a"}, &OutputTextPart{Text: "b"}}},
		{name: "unterminated string", raw: `"abc`, err: "content string: …"},
		{name: "number", raw: `5`, err: "content array: json: cannot unmarshal number…"},
		{name: "array with a bad part", raw: `[{"type":"input_text","text":"a"},{"type":"nope"}]`, err: `unsupported part type "nope"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unmarshalContent([]byte(tc.raw))
			wantErr(t, err, tc.err)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("parts (-want +got):\n%s", diff)
			}
		})
	}
}
