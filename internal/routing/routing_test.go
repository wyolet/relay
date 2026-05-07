package routing_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/routing"
)

// helper builders

func provider(name string) *catalog.Provider {
	return &catalog.Provider{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.ProviderSpec{Kind: catalog.PKOpenAI, DefaultPool: name + "-pool"},
	}
}

func pool(name, providerName string) *catalog.Pool {
	return &catalog.Pool{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.PoolSpec{Provider: providerName},
	}
}

func model(name, providerName string) *catalog.Model {
	return &catalog.Model{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.ModelSpec{Provider: providerName, UpstreamName: name},
	}
}

func route(name string, defaultRoute bool, models ...string) *catalog.Route {
	return &catalog.Route{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.RouteSpec{Default: defaultRoute, Models: models},
	}
}

// catalog fixture shared by most tests:
//   provider: "openai"  pool: "openai-pool"
//   models:   "gpt-4", "gpt-3.5"
//   routes:   "fast" → [gpt-4], "cheap" → [gpt-3.5], "default-route" (default) → [gpt-3.5]
func fixture() *catalog.MemStore {
	return catalog.NewMemStore(
		provider("openai"),
		pool("openai-pool", "openai"),
		model("gpt-4", "openai"),
		model("gpt-3.5", "openai"),
		route("fast", false, "gpt-4"),
		route("cheap", false, "gpt-3.5"),
		route("default-route", true, "gpt-3.5"),
	)
}

func TestHeaderOnly_UsesFirstModelInRoute(t *testing.T) {
	r := routing.New(fixture())
	plan, err := r.Resolve(routing.Request{RouteHeader: "fast"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Model.Metadata.Name != "gpt-4" {
		t.Fatalf("want gpt-4, got %q", plan.Model.Metadata.Name)
	}
}

func TestHeaderAndMatchingBodyModel(t *testing.T) {
	r := routing.New(fixture())
	plan, err := r.Resolve(routing.Request{RouteHeader: "fast", ModelName: "gpt-4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Model.Metadata.Name != "gpt-4" {
		t.Fatalf("want gpt-4, got %q", plan.Model.Metadata.Name)
	}
}

func TestHeaderAndBodyModelNotInRoute_ErrModelNotInRoute(t *testing.T) {
	r := routing.New(fixture())
	_, err := r.Resolve(routing.Request{RouteHeader: "fast", ModelName: "gpt-3.5"})
	if !errors.Is(err, routing.ErrModelNotInRoute) {
		t.Fatalf("want ErrModelNotInRoute, got %v", err)
	}
}

func TestBodyOnly_DirectModelLookup(t *testing.T) {
	r := routing.New(fixture())
	plan, err := r.Resolve(routing.Request{ModelName: "gpt-3.5"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Model.Metadata.Name != "gpt-3.5" {
		t.Fatalf("want gpt-3.5, got %q", plan.Model.Metadata.Name)
	}
}

func TestNoHeaderNoBody_DefaultRouteExists(t *testing.T) {
	r := routing.New(fixture())
	plan, err := r.Resolve(routing.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// default-route points to gpt-3.5
	if plan.Model.Metadata.Name != "gpt-3.5" {
		t.Fatalf("want gpt-3.5, got %q", plan.Model.Metadata.Name)
	}
}

func TestNoHeaderNoBody_NoDefaultRoute_ErrNoModelSpecified(t *testing.T) {
	store := catalog.NewMemStore(
		provider("openai"),
		pool("openai-pool", "openai"),
		model("gpt-4", "openai"),
	)
	r := routing.New(store)
	_, err := r.Resolve(routing.Request{})
	if !errors.Is(err, routing.ErrNoModelSpecified) {
		t.Fatalf("want ErrNoModelSpecified, got %v", err)
	}
}

func TestUnknownRouteHeader_ErrUnknownRoute(t *testing.T) {
	r := routing.New(fixture())
	_, err := r.Resolve(routing.Request{RouteHeader: "nonexistent"})
	if !errors.Is(err, routing.ErrUnknownRoute) {
		t.Fatalf("want ErrUnknownRoute, got %v", err)
	}
}

func TestPlanHasPoolAndSecrets(t *testing.T) {
	secret := &catalog.Secret{
		Metadata: catalog.Metadata{Name: "sk-1"},
		Spec:     catalog.SecretSpec{Provider: "openai"},
	}
	p := pool("openai-pool", "openai")
	p.Spec.Secrets = []string{"sk-1"}
	store := catalog.NewMemStore(
		provider("openai"),
		p,
		model("gpt-4", "openai"),
		secret,
	)
	r := routing.New(store)
	plan, err := r.Resolve(routing.Request{ModelName: "gpt-4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Pool == nil {
		t.Fatal("expected pool to be set")
	}
	if len(plan.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(plan.Secrets))
	}
}
