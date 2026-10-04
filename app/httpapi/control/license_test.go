package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/license"
)

// fakeLicense accepts exactly one value and, like the real service, leaves
// the live license untouched when a value does not verify.
type fakeLicense struct {
	good string
	info license.Info
}

func (f *fakeLicense) Has(feature string) bool {
	for _, have := range f.info.Features {
		if have == feature {
			return true
		}
	}
	return false
}

func (f *fakeLicense) Info() license.Info { return f.info }

func (f *fakeLicense) Set(value string) (license.Info, error) {
	if value != f.good {
		return f.info, huma.Error400BadRequest("license: signature does not verify")
	}
	f.info = license.Info{Licensed: true, Customer: "acme", Features: []string{license.FeatureSSO}}
	return f.info, nil
}

// newLicenseHarness mounts /version + /license with lic wired in. Requests
// carrying X-Test-Admin arrive authenticated.
func newLicenseHarness(t *testing.T, lic license.Service) http.Handler {
	t.Helper()
	d := Deps{Authz: authz.AlwaysAllowAuthenticated{}, License: lic}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("X-Test-Admin") != "" {
				req = req.WithContext(actor.WithActor(req.Context(), &actor.Actor{AdminToken: true}))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("license-test", "0"))
	protect := huma.Middlewares{httpapi.HumaAuth(RequireActor)}
	registerVersion(api, d)
	registerLicense(api, d, protect)
	return r
}

func licenseReq(t *testing.T, h http.Handler, admin bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if admin {
		req.Header.Set("X-Test-Admin", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestVersionCarriesLicenseForAdminsOnly(t *testing.T) {
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	lic := &fakeLicense{good: "good.license.value", info: license.Info{
		Licensed: true, Customer: "acme", ExpiresAt: expires,
		Features: []string{license.FeatureSSO}, Grace: true,
	}}
	h := newLicenseHarness(t, lic)

	var anon struct {
		Version string        `json:"version"`
		License *license.Info `json:"license"`
	}
	w := licenseReq(t, h, false, http.MethodGet, "/version", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &anon); err != nil {
		t.Fatal(err)
	}
	if anon.Version == "" || anon.License != nil {
		t.Fatalf("anonymous /version must omit the license block: %s", w.Body.String())
	}

	var admin struct {
		License *license.Info `json:"license"`
	}
	w = licenseReq(t, h, true, http.MethodGet, "/version", "")
	if err := json.Unmarshal(w.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	if admin.License == nil {
		t.Fatalf("admin /version must carry the license block: %s", w.Body.String())
	}
	if !admin.License.Licensed || admin.License.Customer != "acme" ||
		!admin.License.ExpiresAt.Equal(expires) || !admin.License.Grace ||
		len(admin.License.Features) != 1 {
		t.Fatalf("license block = %+v", *admin.License)
	}
}

func TestGetLicenseShape(t *testing.T) {
	t.Run("community", func(t *testing.T) {
		h := newLicenseHarness(t, &fakeLicense{good: "good"})
		w := licenseReq(t, h, true, http.MethodGet, "/license", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var got license.Info
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Licensed || got.Customer != "" || got.Grace {
			t.Fatalf("community info = %+v", got)
		}
	})

	t.Run("no service wired", func(t *testing.T) {
		h := newLicenseHarness(t, nil)
		w := licenseReq(t, h, true, http.MethodGet, "/license", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"licensed":false`) {
			t.Fatalf("status = %d body = %s, want a community answer", w.Code, w.Body.String())
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		h := newLicenseHarness(t, &fakeLicense{good: "good"})
		if w := licenseReq(t, h, false, http.MethodGet, "/license", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})
}

func TestPutLicenseRejectsAnInvalidKey(t *testing.T) {
	lic := &fakeLicense{good: "good.license.value", info: license.Info{
		Licensed: true, Customer: "incumbent", Features: []string{license.FeatureSSO},
	}}
	h := newLicenseHarness(t, lic)

	w := licenseReq(t, h, true, http.MethodPut, "/license", `{"value":"not-a-license"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}

	// The previous license is still live and still unlocking its features.
	w = licenseReq(t, h, true, http.MethodGet, "/license", "")
	var got license.Info
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Licensed || got.Customer != "incumbent" {
		t.Fatalf("info = %+v, want the previous license intact", got)
	}
	if !lic.Has(license.FeatureSSO) {
		t.Error("a rejected PUT must not revoke the live license's features")
	}
}
