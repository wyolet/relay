package inference

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

// unsupportedTranslator refuses every request as one its wire shape cannot express.
type unsupportedTranslator struct{ stubV1Translator }

func (unsupportedTranslator) SerializeRequest(*pkgrelay.Request) ([]byte, error) {
	return nil, fmt.Errorf("upstream serialize_request: %w: no such mode", pkgrelay.ErrUnsupportedRequest)
}

// A request the upstream shape cannot express is the caller's to fix (400); any other serialize failure is relay's (500). Both are recorded as marshal_request.
func TestDispatch_SerializeFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		upstream pkgrelay.Translator
		status   int
		want     httpapi.APIErrorBody
	}{
		"unsupported request": {unsupportedTranslator{}, http.StatusBadRequest,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "unsupported_request", Message: "upstream serialize_request: not supported by the upstream API: no such mode"}},
		"serializer failure": {stubV1Translator{}, http.StatusInternalServerError,
			httpapi.APIErrorBody{Type: "server_error", Code: "marshal_request", Message: "stub: not implemented"}},
	} {
		t.Run(name, func(t *testing.T) {
			cat, pr := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
			d := buildDeps(t, cat)
			useSpecs(&d, adapter.NewRegistry(
				(&adapter.Spec{Name: adapters.Anthropic, DefaultPath: "/v1/messages", Auth: adapter.AuthStrategy{Header: "x-api-key"}, Translator: tc.upstream}).Build(),
				(&adapter.Spec{Name: adapters.Canonical, Translator: pkgrelay.IdentityTranslator{}}).Build(),
			))
			outcomes := watchOutcomes(&d)

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.Canonical, Body: []byte(`{"model":"test-model","input":"hi"}`), ModelName: "test-model"})

			wantAnswer(t, w, tc.status, tc.want, map[string]string{HeaderOrigin: "relay"})
			wantOutcome(t, outcomes, outcome{Kind: "marshal_request", Message: tc.want.Message})
		})
	}
}
