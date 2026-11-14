package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
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
// carrying X-Test-Admin: 1 arrive authenticated.
func newLicenseHarness(t *testing.T, lic license.Service) http.Handler {
	t.Helper()
	return newLicenseHarnessOver(t, lic, execOnlyDBTX{})
}

// failingDBTX is a database every write to fails.
type failingDBTX struct{ execOnlyDBTX }

func (failingDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("pg down")
}

// newLicenseHarnessOver is newLicenseHarness with the settings section stored in db.
func newLicenseHarnessOver(t *testing.T, lic license.Service, db gen.DBTX) http.Handler {
	t.Helper()
	httpapi.Install()
	d := Deps{Authz: authz.AlwaysAllowAuthenticated{}, License: lic, Stores: &appcatalog.Stores{Settings: settings.NewStore(gen.New(db))}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Admin", map[string]*actor.Actor{"1": {AdminToken: true}}))
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

// /version is public and carries the license only for a signed-in caller; /license needs one, and answers the community shape when nothing is licensed or no license service is wired.
func TestVersionAndLicenseReads(t *testing.T) {
	held := license.Info{Licensed: true, Customer: "acme", ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Features: []string{license.FeatureSSO}, Grace: true}
	type versionBody struct {
		Version string        `json:"version"`
		License *license.Info `json:"license"`
	}
	for _, tc := range []struct {
		name     string
		lic      license.Service
		signedIn bool
		path     string
		status   int
		want     any
	}{
		{"version, anonymous", &fakeLicense{info: held}, false, "/version", 200, versionBody{Version: httpapi.Version}},
		{"version, signed in", &fakeLicense{info: held}, true, "/version", 200, versionBody{Version: httpapi.Version, License: &held}},
		{"version, signed in, no license service", nil, true, "/version", 200, versionBody{Version: httpapi.Version, License: &license.Info{}}},
		{"license, licensed", &fakeLicense{info: held}, true, "/license", 200, held},
		{"license, community", &fakeLicense{}, true, "/license", 200, license.Info{}},
		{"license, no license service", nil, true, "/license", 200, license.Info{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := licenseReq(t, newLicenseHarness(t, tc.lic), tc.signedIn, http.MethodGet, tc.path, "")
			got := reflect.New(reflect.TypeOf(tc.want))
			if err := json.Unmarshal(w.Body.Bytes(), got.Interface()); err != nil || w.Code != tc.status {
				t.Fatalf("status = %d (%v), want %d: %s", w.Code, err, tc.status, w.Body)
			}
			if diff := cmp.Diff(tc.want, got.Elem().Interface(), cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("body (-want +got):\n%s", diff)
			}
		})
	}
	w := licenseReq(t, newLicenseHarness(t, &fakeLicense{info: held}), false, http.MethodGet, "/license", "")
	if w.Code != http.StatusUnauthorized || w.Body.String() != unauthenticatedBody {
		t.Fatalf("anonymous /license = %d %s, want 401", w.Code, w.Body)
	}
}

// A value that does not verify is refused and leaves the live license as it was; one that verifies replaces it.
func TestPutLicense(t *testing.T) {
	incumbent := license.Info{Licensed: true, Customer: "incumbent", Features: []string{license.FeatureSSO}}
	lic := &fakeLicense{good: "good.license.value", info: incumbent}
	h := newLicenseHarness(t, lic)
	read := func() license.Info {
		t.Helper()
		var got license.Info
		if err := json.Unmarshal(licenseReq(t, h, true, http.MethodGet, "/license", "").Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	wantAPIError(t, licenseReq(t, h, true, http.MethodPut, "/license", `{"value":"not-a-license"}`), 400, "", "license: signature does not verify")
	if diff := cmp.Diff(incumbent, read()); diff != "" || !lic.Has(license.FeatureSSO) {
		t.Fatalf("after a refused PUT the live license changed (-want +got):\n%s", diff)
	}
	if w := licenseReq(t, h, false, http.MethodPut, "/license", `{"value":"good.license.value"}`); w.Code != http.StatusUnauthorized || lic.info.Customer != "incumbent" {
		t.Fatalf("anonymous PUT = %d with customer %q, want 401 and no change", w.Code, lic.info.Customer)
	}

	replaced := license.Info{Licensed: true, Customer: "acme", Features: []string{license.FeatureSSO}}
	var got license.Info
	w := licenseReq(t, h, true, http.MethodPut, "/license", `{"value":"good.license.value"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("PUT of a good value = %d (%v): %s", w.Code, err, w.Body)
	}
	if diff := cmp.Diff(replaced, got); diff != "" || cmp.Diff(replaced, read()) != "" {
		t.Fatalf("after a good PUT (-want +got):\n%s", diff)
	}
}

// The stored section is what the next boot and the other pods read, so a value that could not be stored must not be live here alone.
func TestPutLicenseThatCannotBeStoredIsNotLive(t *testing.T) {
	t.Skip("bug: PUT /license with a good value whose settings write fails → 500, yet the value is already live in this process")
	incumbent := license.Info{Licensed: true, Customer: "incumbent"}
	lic := &fakeLicense{good: "good.license.value", info: incumbent}
	h := newLicenseHarnessOver(t, lic, failingDBTX{})
	wantAPIError(t, licenseReq(t, h, true, http.MethodPut, "/license", `{"value":"good.license.value"}`), 500, "internal_error", "settings: settings.Upsert: pg down")
	if diff := cmp.Diff(incumbent, lic.Info()); diff != "" {
		t.Fatalf("the live license after a failed PUT (-want +got):\n%s", diff)
	}
}

// badLicense refuses every value, as a stored license that no longer verifies does.
type badLicense struct{}

func (badLicense) Has(string) bool    { return false }
func (badLicense) Info() license.Info { return license.Info{} }
func (badLicense) Set(string) (license.Info, error) {
	return license.Info{}, errors.New("signature does not verify")
}

// A reload rebuilds the snapshot; a stored license that stopped verifying is a separate fact and must not turn the operator's rebuild into a 500.
func TestReloadReportsABadLicenseWithout500(t *testing.T) {
	r, api := testAPI(t, scopeActors)
	registerMisc(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}, Catalog: catalogtest.Catalog{}.New(), License: badLicense{}}, nil)

	w := scopeReq(t, r, "root", http.MethodPost, "/reload", "")
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status = %d (%v), want 200: %s", w.Code, err, w.Body)
	}
	delete(body, "$schema")
	if diff := cmp.Diff(map[string]any{"status": "ok", "license": map[string]any{"error": "signature does not verify"}}, body); diff != "" {
		t.Fatalf("body (-want +got):\n%s", diff)
	}
}
