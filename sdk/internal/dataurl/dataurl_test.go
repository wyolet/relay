package dataurl

import "testing"

func TestBase64(t *testing.T) {
	cases := []struct {
		name, url, mediaType, data string
		wantErr                    bool
	}{
		{"base64 payload kept as is", "data:image/png;base64,iVBO", "image/png", "iVBO", false},
		{"parameters dropped from the media type", "data:image/svg+xml;charset=utf-8;base64,PHN2Zz4=", "image/svg+xml", "PHN2Zz4=", false},
		{"percent-encoded payload re-encoded", "data:image/svg+xml;charset=utf-8,%3Csvg%3E+", "image/svg+xml", "PHN2Zz4r", false},
		{"plain payload re-encoded", "data:image/png,abc", "image/png", "YWJj", false},
		{"no media type", "data:,a", "", "YQ==", false},
		{"no comma", "data:image/png;base64", "", "", true},
		{"bad percent escape", "data:image/png,%zz", "", "", true},
		{"not a data URL", "https://example.com/a.png", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mediaType, data, err := Base64(c.url)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if mediaType != c.mediaType || data != c.data {
				t.Errorf("got (%q, %q), want (%q, %q)", mediaType, data, c.mediaType, c.data)
			}
		})
	}
}
