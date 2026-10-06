package settings

import "testing"

type oneSection map[string]any

func (s oneSection) Setting(section string) (any, bool) {
	v, ok := s[section]
	return v, ok
}

func TestOTLPReceiverDefaultsToOff(t *testing.T) {
	for name, r := range map[string]Reader{
		"no reader":        nil,
		"section absent":   oneSection{},
		"section mistyped": oneSection{SectionOTLPReceiver: "enabled"},
	} {
		if got := OTLPReceiverFrom(r); got.Enabled || got.CaptureContent {
			t.Errorf("%s: %+v, want the receiver and content capture off", name, got)
		}
	}
	sec, ok := Lookup(SectionOTLPReceiver)
	if !ok {
		t.Fatal("section not registered")
	}
	if def, ok := sec.Defaults().(*OTLPReceiver); !ok || def.Enabled || def.CaptureContent {
		t.Errorf("registered default = %+v, want both switches off", sec.Defaults())
	}
}

func TestOTLPReceiverDecodesContentCapture(t *testing.T) {
	sec, _ := Lookup(SectionOTLPReceiver)
	v, err := sec.Decode([]byte(`{"enabled":true,"captureContent":true}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := OTLPReceiverFrom(oneSection{SectionOTLPReceiver: v})
	if !got.Enabled || !got.CaptureContent {
		t.Errorf("decoded = %+v, want both switches on", got)
	}

	// The body that enabled the receiver before content capture existed still does, and leaves capture off.
	v, err = sec.Decode([]byte(`{"enabled":true}`))
	if err != nil {
		t.Fatalf("Decode without captureContent: %v", err)
	}
	if got := OTLPReceiverFrom(oneSection{SectionOTLPReceiver: v}); !got.Enabled || got.CaptureContent {
		t.Errorf("decoded = %+v, want the receiver on and content capture off", got)
	}
}
