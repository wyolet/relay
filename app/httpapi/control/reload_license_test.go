package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	applicense "github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// badLicense refuses every value it is handed, the way a stored licence that
// no longer verifies does.
type badLicense struct{}

func (badLicense) Has(string) bool       { return false }
func (badLicense) Info() applicense.Info { return applicense.Info{} }
func (badLicense) Set(string) (applicense.Info, error) {
	return applicense.Info{}, errors.New("signature does not verify")
}

// A reload rebuilds the snapshot; a stored licence that stopped verifying is
// a separate fact and must not turn the operator's rebuild into a 500.
func TestReloadReportsABadLicenseWithout500(t *testing.T) {
	cat := appcatalog.New(
		tokenList[provider.Provider]{}, tokenList[host.Host]{}, tokenList[policy.Policy]{},
		tokenList[model.Model]{}, tokenList[hostkey.HostKey]{}, tokenList[ratelimit.RateLimit]{},
		tokenList[key.Key]{}, tokenList[pricing.Pricing]{}, tokenList[binding.Binding]{},
	)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("reload-test", "0"))
	registerMisc(api, Deps{
		Authz:   authz.AlwaysAllowAuthenticated{},
		Catalog: cat,
		License: badLicense{},
	}, nil)

	w := scopeReq(t, r, "root", http.MethodPost, "/reload", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var body struct {
		Status  string `json:"status"`
		License *struct {
			Error string `json:"error"`
		} `json:"license"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.License == nil || body.License.Error == "" {
		t.Fatalf("body = %s, want a license.error", w.Body)
	}
}
