package manifest

func registerCatalogKinds(p *Parser) {
	p.Register("Provider", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec ProviderSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Provider: &ProviderDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
	p.Register("Host", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec HostSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Host: &HostDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
	p.Register("Model", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec ModelSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Model: &ModelDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
	p.Register("Policy", KindDecoder{
		Decode: func(env *Envelope) (Document, error) {
			var spec PolicySpec
			if err := env.Spec.Decode(&spec); err != nil {
				return Document{}, err
			}
			return Document{Policy: &PolicyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
		},
		RemovedFields: map[string]string{
			"hostKeys":              "pools",
			"rateLimit":             "rateLimits: [{ref: <name>}]",
			"rlBindings":            "rateLimits: [{ref: <name>, models: [...]}]",
			"keySelection":          "a Pool's selection",
			"modelIds":              "models (model refs)",
			"credentials":           "pools (a Pool of the one host key)",
			"includeDeprecated":     "nothing: models grant deprecated models like any other",
			"payloadLoggingEnabled": "payloadLogging on the accounts the policy is bound to",
		},
		ReshapedHint: "hostKeys, rateLimit, rlBindings and keySelection are gone: link pools or credentials, rateLimits: [{ref, models}] and budgets",
	})
	p.Register("RateLimit", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec RateLimitSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{RateLimit: &RateLimitDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
	p.Register("Pricing", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec PricingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Pricing: &PricingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
	p.Register("HostBinding", KindDecoder{Decode: func(env *Envelope) (Document, error) {
		var spec HostBindingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{HostBinding: &HostBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil
	}})
}
