package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wyolet/relay/pkg/configstore"
)

type fakeStore struct {
	models []*configstore.Model
}

func (f *fakeStore) Models() []*configstore.Model                                       { return f.models }
func (f *fakeStore) ProviderByName(string) (*configstore.Provider, bool)               { return nil, false }
func (f *fakeStore) ModelByName(string) (*configstore.Model, bool)                     { return nil, false }
func (f *fakeStore) RouteByName(string) (*configstore.Route, bool)                     { return nil, false }
func (f *fakeStore) RateLimitByName(string) (*configstore.RateLimit, bool)             { return nil, false }
func (f *fakeStore) SecretByName(string) (*configstore.Secret, bool)                   { return nil, false }
func (f *fakeStore) PoolByName(string) (*configstore.Pool, bool)                       { return nil, false }
func (f *fakeStore) Providers() []*configstore.Provider                                { return nil }
func (f *fakeStore) Routes() []*configstore.Route                                      { return nil }
func (f *fakeStore) RateLimits() []*configstore.RateLimit                              { return nil }
func (f *fakeStore) Secrets() []*configstore.Secret                                    { return nil }
func (f *fakeStore) Pools() []*configstore.Pool                                        { return nil }
func (f *fakeStore) DefaultProvider() *configstore.Provider                            { return nil }
func (f *fakeStore) DefaultRoute() *configstore.Route                                  { return nil }
func (f *fakeStore) ProviderForModel(string) (*configstore.Provider, bool)             { return nil, false }
func (f *fakeStore) SecretsForPool(*configstore.Pool) []*configstore.Secret            { return nil }
func (f *fakeStore) RateLimitsForRequest(*configstore.Provider, *configstore.Pool, *configstore.Model, *configstore.Secret) []configstore.ResolvedRule {
	return nil
}

func makeModel(name, provider string) *configstore.Model {
	return &configstore.Model{
		Metadata: configstore.Metadata{Name: name},
		Spec:     configstore.ModelSpec{Provider: provider},
	}
}

func TestListModels_HappyPath(t *testing.T) {
	store := &fakeStore{models: []*configstore.Model{
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
	store := &fakeStore{models: []*configstore.Model{}}
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
