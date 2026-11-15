package bitwarden

import (
	"net/http"
	"regexp"
	"testing"
)

func TestNewClient(t *testing.T) {
	c := newClient(Config{BaseURL: "https://vault.example/", Email: "e", MasterPassword: "p", ClientID: "id", ClientSecret: "secret"})
	if c.baseURL != "https://vault.example" || c.email != "e" || c.password != "p" || c.clientID != "id" || c.clientSecret != "secret" || c.deviceName != "wyolet-secret" {
		t.Errorf("client = %+v", c)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(c.deviceID) {
		t.Errorf("generated device id = %q, want 32 hex characters", c.deviceID)
	}
	if again := newClient(Config{}); again.deviceID == c.deviceID {
		t.Errorf("two clients share the generated device id %q", c.deviceID)
	}
	if tls := c.httpClient.Transport.(*http.Transport).TLSClientConfig; tls != nil && tls.InsecureSkipVerify {
		t.Error("certificate verification is off by default")
	}

	c = newClient(Config{DeviceID: "dev-1", DeviceName: "my-service", InsecureSkipVerify: true})
	if c.deviceID != "dev-1" || c.deviceName != "my-service" {
		t.Errorf("configured device = %q %q", c.deviceID, c.deviceName)
	}
	if tls := c.httpClient.Transport.(*http.Transport).TLSClientConfig; tls == nil || !tls.InsecureSkipVerify {
		t.Error("InsecureSkipVerify did not reach the transport")
	}
	if tls := http.DefaultTransport.(*http.Transport).TLSClientConfig; tls != nil && tls.InsecureSkipVerify {
		t.Error("InsecureSkipVerify leaked into http.DefaultTransport")
	}
}
