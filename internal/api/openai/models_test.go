package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wyolet/relay/internal/catalog"
)

type fakeStore struct {
	models []*catalog.Model
}

func (f *fakeStore) Models() []*catalog.Model                                       { return f.models }
func (f *fakeStore) ProviderByName(string) (*catalog.Provider, bool)               { return nil, false }
func (f *fakeStore) ModelByName(string) (*catalog.Model, bool)                     { return nil, false }
func (f *fakeStore) RouteByName(string) (*catalog.Route, bool)                     { return nil, false }
func (f *fakeStore) RateLimitByName(string) (*catalog.RateLimit, bool)             { return nil, false }
func (f *fakeStore) SecretByName(string) (*catalog.Secret, bool)                   { return nil, false }
func (f *fakeStore) PolicyByName(string) (*catalog.Policy, bool)                       { return nil, false }
func (f *fakeStore) Providers() []*catalog.Provider                                { return nil }
func (f *fakeStore) Routes() []*catalog.Route                                      { return nil }
func (f *fakeStore) RateLimits() []*catalog.RateLimit                              { return nil }
func (f *fakeStore) Secrets() []*catalog.Secret                                    { return nil }
func (f *fakeStore) Policies() []*catalog.Policy                                        { return nil }
func (f *fakeStore) DefaultProvider() *catalog.Provider                            { return nil }
func (f *fakeStore) DefaultRoute() *catalog.Route                                  { return nil }
func (f *fakeStore) ProviderForModel(string) (*catalog.Provider, bool)             { return nil, false }
func (f *fakeStore) SecretsForPolicy(*catalog.Policy) []*catalog.Secret            { return nil }
func (f *fakeStore) RateLimitsForRequest(*catalog.Provider, *catalog.Policy, *catalog.Model, *catalog.Secret) []catalog.ResolvedRule {
	return nil
}
func (f *fakeStore) EffectivePricing(string) (*catalog.Pricing, bool) { return nil, false }
func (f *fakeStore) RelayKeyByName(string) (*catalog.RelayKey, bool)  { return nil, false }
func (f *fakeStore) RelayKeyByHash(string) (*catalog.RelayKey, bool)  { return nil, false }
func (f *fakeStore) RelayKeys() []*catalog.RelayKey                   { return nil }
func (f *fakeStore) Passthrough() *catalog.Passthrough                { return catalog.DefaultPassthrough() }

func (f *fakeStore) ProviderByID(string) (*catalog.Provider, bool)   { return nil, false }
func (f *fakeStore) ModelByID(string) (*catalog.Model, bool)         { return nil, false }
func (f *fakeStore) RouteByID(string) (*catalog.Route, bool)         { return nil, false }
func (f *fakeStore) RateLimitByID(string) (*catalog.RateLimit, bool) { return nil, false }
func (f *fakeStore) SecretByID(string) (*catalog.Secret, bool)       { return nil, false }
func (f *fakeStore) PolicyByID(string) (*catalog.Policy, bool)       { return nil, false }
func (f *fakeStore) RelayKeyByID(string) (*catalog.RelayKey, bool)   { return nil, false }

func makeModel(name, provider string) *catalog.Model {
	return &catalog.Model{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.ModelSpec{Provider: provider},
	}
}

func TestListModels_HappyPath(t *testing.T) {
	store := &fakeStore{models: []*catalog.Model{
		makeModel("gemma4", "dev-ollama"),
		makeModel("gemma3-27b", "dev-ollama"),
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	ListModels(store)(rec, req)

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]interface{}{
		"object": "list",
		"data": []interface{}{
			map[string]interface{}{"id": "gemma4", "object": "model", "created": float64(0), "owned_by": "dev-ollama"},
			map[string]interface{}{"id": "gemma3-27b", "object": "model", "created": float64(0), "owned_by": "dev-ollama"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestListModels_EmptyCatalog(t *testing.T) {
	store := &fakeStore{models: []*catalog.Model{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	ListModels(store)(rec, req)

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, ok := got["data"].([]interface{})
	if !ok {
		t.Fatalf("data field missing or wrong type: %v", got["data"])
	}
	if len(data) != 0 {
		t.Errorf("expected empty data array, got %v", data)
	}
}

func TestListModels_ContentType(t *testing.T) {
	store := &fakeStore{}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	ListModels(store)(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
