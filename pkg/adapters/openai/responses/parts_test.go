package responses

import (
	"encoding/json"
	"testing"
)

func TestTextPartRoundTrip(t *testing.T) {
	wire := `{"type":"input_text","text":"hello"}`
	p, err := unmarshalPart([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	tp, ok := p.(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", p)
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
	b, _ := json.Marshal(tp)
	p2, err := unmarshalPart(b)
	if err != nil {
		t.Fatal(err)
	}
	if p2.(*TextPart).Text != "hello" {
		t.Error("text mismatch after round-trip")
	}
}

func TestImagePartRoundTrip(t *testing.T) {
	wire := `{"type":"input_image","image_url":"https://example.com/img.png","detail":"high"}`
	p, err := unmarshalPart([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := p.(*ImagePart)
	if !ok {
		t.Fatalf("expected *ImagePart, got %T", p)
	}
	if ip.Detail != "high" {
		t.Errorf("detail: %q", ip.Detail)
	}
	b, _ := json.Marshal(ip)
	p2, _ := unmarshalPart(b)
	if p2.(*ImagePart).ImageURL != ip.ImageURL {
		t.Error("image_url mismatch")
	}
}

func TestFilePartRoundTrip(t *testing.T) {
	wire := `{"type":"input_file","file_id":"file_abc","filename":"doc.pdf"}`
	p, err := unmarshalPart([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := p.(*FilePart)
	if !ok {
		t.Fatalf("expected *FilePart, got %T", p)
	}
	if fp.FileID != "file_abc" {
		t.Errorf("file_id: %q", fp.FileID)
	}
	b, _ := json.Marshal(fp)
	p2, _ := unmarshalPart(b)
	if p2.(*FilePart).Filename != fp.Filename {
		t.Error("filename mismatch")
	}
}

func TestOutputTextPartRoundTrip(t *testing.T) {
	wire := `{"type":"output_text","text":"answer","annotations":[],"logprobs":null}`
	p, err := unmarshalPart([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	otp, ok := p.(*OutputTextPart)
	if !ok {
		t.Fatalf("expected *OutputTextPart, got %T", p)
	}
	if otp.Text != "answer" {
		t.Errorf("text: %q", otp.Text)
	}
}

func TestRefusalPartRoundTrip(t *testing.T) {
	wire := `{"type":"refusal","refusal":"I cannot do that"}`
	p, err := unmarshalPart([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := p.(*RefusalPart)
	if !ok {
		t.Fatalf("expected *RefusalPart, got %T", p)
	}
	if rp.Refusal != "I cannot do that" {
		t.Errorf("refusal: %q", rp.Refusal)
	}
	b, _ := json.Marshal(rp)
	p2, _ := unmarshalPart(b)
	if p2.(*RefusalPart).Refusal != rp.Refusal {
		t.Error("refusal mismatch after round-trip")
	}
}

func TestStringVsArrayContentNormalization(t *testing.T) {
	t.Run("string normalizes to TextPart", func(t *testing.T) {
		parts, err := unmarshalContent([]byte(`"hi there"`))
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
		if tp.Text != "hi there" {
			t.Errorf("text: %q", tp.Text)
		}
	})

	t.Run("array preserves order", func(t *testing.T) {
		raw := `[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"https://x.com/img.png"}]`
		parts, err := unmarshalContent([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 2 {
			t.Fatalf("expected 2 parts, got %d", len(parts))
		}
		if parts[0].PartType() != PartTypeInputText {
			t.Errorf("parts[0]: %v", parts[0].PartType())
		}
		if parts[1].PartType() != PartTypeInputImage {
			t.Errorf("parts[1]: %v", parts[1].PartType())
		}
	})
}

func TestAnnotationRoundTrip(t *testing.T) {
	t.Run("url_citation", func(t *testing.T) {
		wire := `{"type":"url_citation","start_index":0,"end_index":5,"url":"https://example.com","title":"Example"}`
		a, err := unmarshalAnnotation([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		uc, ok := a.(*URLCitationAnnotation)
		if !ok {
			t.Fatalf("expected *URLCitationAnnotation, got %T", a)
		}
		if uc.URL != "https://example.com" {
			t.Errorf("url: %q", uc.URL)
		}
		b, _ := json.Marshal(uc)
		a2, _ := unmarshalAnnotation(b)
		if a2.(*URLCitationAnnotation).Title != uc.Title {
			t.Error("title mismatch")
		}
	})

	t.Run("file_citation", func(t *testing.T) {
		wire := `{"type":"file_citation","file_id":"file_xyz","index":3}`
		a, err := unmarshalAnnotation([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		fc, ok := a.(*FileCitationAnnotation)
		if !ok {
			t.Fatalf("expected *FileCitationAnnotation, got %T", a)
		}
		if fc.FileID != "file_xyz" {
			t.Errorf("file_id: %q", fc.FileID)
		}
	})

	t.Run("unknown annotation type preserved as RawAnnotation", func(t *testing.T) {
		wire := `{"type":"container_file_citation","file_id":"f_1","container_id":"c_1"}`
		a, err := unmarshalAnnotation([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		ra, ok := a.(*RawAnnotation)
		if !ok {
			t.Fatalf("expected *RawAnnotation, got %T", a)
		}
		if ra.Type != "container_file_citation" {
			t.Errorf("type: %q", ra.Type)
		}
	})
}
