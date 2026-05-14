package inference

import (
	"context"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	pkganthropic "github.com/wyolet/relay/pkg/api/anthropic"
)

func registerMessages(api huma.API, d Deps, mw huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "messages",
		Method:      http.MethodPost,
		Path:        "/v1/messages",
		Summary:     "Create a message (Anthropic-compatible)",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{400, 401, 403, 404, 429, 500, 502, 503},
	}, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			r, w := humachi.Unwrap(hctx)
			handleMessages(d, w, r)
		}}, nil
	})
}

// handleMessages mirrors handleChat with the Anthropic parser and adapter.
func handleMessages(d Deps, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	cls := ClassificationFrom(ctx)
	if cls.Mode == ModeProxyAuthed || cls.Mode == ModeProxyAnonymous {
		handleProxy(d, w, r, adapter.Anthropic)
		return
	}

	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}

	req, err := pkganthropic.Parse(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "parse_error", err.Error())
		return
	}

	plan, err := d.Resolver.Resolve(routing.Request{
		ModelName: req.Model,
		RelayKey:  rk,
	})
	if err != nil {
		mapRoutingErr(w, err)
		return
	}

	if plan.HostBinding.Adapter != adapter.Anthropic {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "adapter_mismatch",
			"model is not served via the Anthropic shape on this host; use /v1/chat/completions if applicable")
		return
	}

	ad, ok := d.Adapters[plan.HostBinding.Adapter]
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(plan.HostBinding.Adapter))
		return
	}

	preq := &pipeline.Request{
		Body:        body,
		Headers:     r.Header,
		HostBaseURL: plan.Host.Spec.BaseURL,
		Adapter:     ad,
		Policy:      plan.Policy,
		Model:       plan.Model,
		Host:        plan.Host,
		Provider:    plan.Provider,
		Keys:        plan.Keys,
		RateScope:   plan.Policy.Meta.Name,
		Rules:       plan.Rules,
		ModelName:   plan.Model.Meta.Name,
	}

	result, err := d.Pipeline.Run(ctx, preq)
	if err != nil {
		mapPipelineErr(w, err)
		return
	}
	defer result.Body.Close()

	for k, vs := range result.Headers {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(result.Status)
	_, _ = io.Copy(w, result.Body)
}
