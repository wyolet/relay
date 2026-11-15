package inference

import (
	"sync"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/internal/adapterspecs"
	"github.com/wyolet/relay/pkg/adapters"
)

var productionSpecs = sync.OnceValue(func() *adapter.Registry { return adapter.NewRegistry(adapterspecs.Specs()...) })

// productionSpec returns the spec relay serves for name, the tests' source of real vendor translators, token extractors and opt-ins.
func productionSpec(name adapters.Name) *adapter.Spec {
	s := productionSpecs().Spec(name)
	if s == nil {
		panic("no spec registered for " + string(name))
	}
	return s
}
