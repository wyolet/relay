package v1

import "testing"

type keepAliveTranslator struct {
	IdentityTranslator
	frame string
}

func (k keepAliveTranslator) KeepAliveFrame() []byte { return []byte(k.frame) }

func TestKeepAliveFrameFor(t *testing.T) {
	const comment = ": keepalive\n\n"
	tests := []struct {
		name string
		tr   Translator
		want string
	}{
		{"shape declares its own frame", keepAliveTranslator{frame: "event: ping\ndata: {}\n\n"}, "event: ping\ndata: {}\n\n"},
		{"shape declares an empty frame", keepAliveTranslator{}, comment},
		{"shape declares none", IdentityTranslator{}, comment},
		{"nil translator", nil, comment},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(KeepAliveFrameFor(tc.tr)); got != tc.want {
				t.Errorf("frame = %q, want %q", got, tc.want)
			}
		})
	}
}
