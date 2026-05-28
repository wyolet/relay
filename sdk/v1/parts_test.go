package v1

import (
	"encoding/json"
	"testing"
)

func TestTextPartRoundTrip(t *testing.T) {
	p := &TextPart{Text: "hello"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	tp, ok := p2.(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", p2)
	}
	if tp.Text != p.Text {
		t.Errorf("text: got %q, want %q", tp.Text, p.Text)
	}
}

func TestImagePartRoundTrip(t *testing.T) {
	p := &ImagePart{ImageURL: "https://example.com/img.png", Detail: "high"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := p2.(*ImagePart)
	if !ok {
		t.Fatalf("expected *ImagePart, got %T", p2)
	}
	if ip.ImageURL != p.ImageURL {
		t.Errorf("image_url: got %q, want %q", ip.ImageURL, p.ImageURL)
	}
	if ip.Detail != p.Detail {
		t.Errorf("detail: got %q, want %q", ip.Detail, p.Detail)
	}
}

func TestFilePartRoundTrip(t *testing.T) {
	p := &FilePart{
		FileURL:   "https://example.com/doc.pdf",
		Filename:  "doc.pdf",
		MediaType: "application/pdf",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := p2.(*FilePart)
	if !ok {
		t.Fatalf("expected *FilePart, got %T", p2)
	}
	if fp.FileURL != p.FileURL {
		t.Errorf("file_url mismatch")
	}
	if fp.Filename != p.Filename {
		t.Errorf("filename mismatch")
	}
	if fp.MediaType != p.MediaType {
		t.Errorf("media_type mismatch")
	}
}

func TestOutputTextPartRoundTrip(t *testing.T) {
	p := &OutputTextPart{Text: "the answer is 42"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := p2.(*OutputTextPart)
	if !ok {
		t.Fatalf("expected *OutputTextPart, got %T", p2)
	}
	if op.Text != p.Text {
		t.Errorf("text: got %q, want %q", op.Text, p.Text)
	}
}

func TestOutputTextPartWithAnnotations(t *testing.T) {
	p := &OutputTextPart{
		Text: "see source",
		Annotations: []Annotation{
			&URLCitationAnnotation{
				StartIndex: 4,
				EndIndex:   10,
				URL:        "https://example.com",
				Title:      "Example",
			},
			&TextCitationAnnotation{
				StartIndex: 0,
				EndIndex:   3,
			},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	op := p2.(*OutputTextPart)
	if len(op.Annotations) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(op.Annotations))
	}
	if op.Annotations[0].AnnotationType() != "url_citation" {
		t.Errorf("annotation[0] type: %s", op.Annotations[0].AnnotationType())
	}
	if op.Annotations[1].AnnotationType() != "text_citation" {
		t.Errorf("annotation[1] type: %s", op.Annotations[1].AnnotationType())
	}
}

func TestRawAnnotationPreservesUnknownType(t *testing.T) {
	raw := []byte(`{"type":"custom_citation","extra":"data"}`)
	a, err := unmarshalAnnotation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.AnnotationType() != "custom_citation" {
		t.Errorf("type: %q", a.AnnotationType())
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(raw) {
		t.Errorf("raw annotation not preserved: got %s, want %s", b, raw)
	}
}

func TestStringContentNormalization(t *testing.T) {
	parts, err := unmarshalContent([]byte(`"hello"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	tp, ok := parts[0].(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", parts[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}
