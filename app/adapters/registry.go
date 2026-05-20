package adapters

// Registry maps adapter Name → Translator. Built once at boot in
// cmd/relay/main.go and passed into the inference layer via Deps.
// Lookup must be cheap (called per request); a plain map is fine.
type Registry map[Name]Translator

// Get returns the Translator for n or nil if unregistered.
func (r Registry) Get(n Name) Translator {
	if r == nil {
		return nil
	}
	return r[n]
}

// Has reports whether n is registered.
func (r Registry) Has(n Name) bool {
	if r == nil {
		return false
	}
	_, ok := r[n]
	return ok
}
