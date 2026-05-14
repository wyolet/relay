// Package modelref parses the catalog reference DSL used by Policy
// grants and the /catalog/resolve admin endpoint.
//
// Grammar (provider-anchored, no bare-model form):
//
//	ref ::= provider                              "anthropic"              → all anthropic models, all hosts
//	      | provider "/" "*"                      "anthropic/*"            → same as above
//	      | provider "/" model                    "anthropic/claude-opus"  → that model on any host
//	      | provider "/" model "@" "*"            "anthropic/claude-opus@*" → same
//	      | provider "/" model "@" host           "anthropic/claude-opus@bedrock" → that model on that host only
//
// The `*` token is literal — only meaningful in the model and host
// positions. No prefix/suffix globbing.
//
// Slugs follow DNS-1123 (matches meta.Metadata.Name validation): lower
// alphanumerics + hyphen, max 63 chars, must start and end with an
// alphanumeric.
package modelref

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Ref is the parsed form of a ref string. The string itself is preserved
// in Raw so admin UIs can echo it verbatim alongside the expansion.
type Ref struct {
	Raw      string
	Provider string // always set
	// Model is the model slug, "" when the ref is provider-only or
	// provider/*.
	Model string
	// Host is the host slug, "" when the ref omits @host or uses @*.
	Host string
	// ModelWildcard is true when the model position is "*" or absent
	// (i.e. all models for the provider).
	ModelWildcard bool
	// HostWildcard is true when the host position is "*" or absent
	// (i.e. all hosts serving the matched model). False when the ref is
	// model-less (provider-only) — in that case Host has no meaning.
	HostWildcard bool
}

// Kind classifies a Ref for the resolve API response.
type Kind string

const (
	KindProvider Kind = "provider" // provider-only or provider/*
	KindModel    Kind = "model"    // provider/model or provider/model@*
	KindBinding  Kind = "binding"  // provider/model@host (concrete host)
)

// Kind returns the classification of this Ref.
func (r Ref) Kind() Kind {
	if r.ModelWildcard {
		return KindProvider
	}
	if r.HostWildcard {
		return KindModel
	}
	return KindBinding
}

var (
	// slug matches DNS-1123 labels (allowing leading/trailing alnum,
	// hyphens within). 1..63 chars.
	slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

	// ErrEmpty is returned for an empty input string.
	ErrEmpty = errors.New("modelref: empty ref")
)

// SyntaxError describes a malformed ref. Carries the raw input + a
// human-friendly explanation suitable for surfacing in 400 responses.
type SyntaxError struct {
	Raw    string
	Reason string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("modelref %q: %s", e.Raw, e.Reason)
}

// Parse turns a ref string into a Ref. Returns ErrEmpty on "" and
// *SyntaxError on a malformed input. The parser never touches the
// catalog — it only enforces grammar.
func Parse(s string) (Ref, error) {
	if s == "" {
		return Ref{}, ErrEmpty
	}
	ref := Ref{Raw: s}

	// Optional @host suffix.
	hostPart := ""
	if i := strings.LastIndex(s, "@"); i >= 0 {
		hostPart = s[i+1:]
		s = s[:i]
		if hostPart == "" {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "trailing @ requires a host slug or *"}
		}
	}

	// provider[/model]
	slash := strings.IndexByte(s, '/')
	switch {
	case slash < 0:
		// provider only — model and host wildcarded by absence.
		ref.Provider = s
		ref.ModelWildcard = true
		ref.HostWildcard = true
	case slash == 0:
		return ref, &SyntaxError{Raw: ref.Raw, Reason: "leading / — provider is required"}
	default:
		ref.Provider = s[:slash]
		ref.Model = s[slash+1:]
		if ref.Model == "" {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "trailing / requires a model slug or *"}
		}
		if ref.Model == "*" {
			ref.Model = ""
			ref.ModelWildcard = true
			ref.HostWildcard = true
		}
	}

	// Validate provider segment.
	if !slugRe.MatchString(ref.Provider) {
		return ref, &SyntaxError{Raw: ref.Raw, Reason: "provider must be a DNS-1123 slug"}
	}

	// Model segment (only when not wildcarded).
	if !ref.ModelWildcard {
		if !slugRe.MatchString(ref.Model) {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "model must be a DNS-1123 slug or *"}
		}
	}

	// Host suffix handling.
	if hostPart != "" {
		if ref.ModelWildcard {
			// "provider@host" and "provider/*@host" are both rejected —
			// the @host suffix has no meaning when no specific model is
			// named. Operators who want "host filter only" can write
			// per-model entries.
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "@host is only valid after an explicit model slug"}
		}
		if hostPart == "*" {
			ref.HostWildcard = true
		} else {
			if !slugRe.MatchString(hostPart) {
				return ref, &SyntaxError{Raw: ref.Raw, Reason: "host must be a DNS-1123 slug or *"}
			}
			ref.Host = hostPart
		}
	} else if !ref.ModelWildcard {
		// provider/model with no @host → all hosts.
		ref.HostWildcard = true
	}

	return ref, nil
}

// MustParse is the panic-on-error variant used by tests and seeds where
// the ref is hard-coded and a parse failure is a build error.
func MustParse(s string) Ref {
	r, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

// Matches reports whether this Ref allows the (provider, model, host)
// binding triple. All three inputs are catalog slugs (Meta.Name). The
// predicate is strict equality with wildcard tolerance:
//
//   - Provider must match exactly.
//   - Model: any slug if ModelWildcard, else exact match.
//   - Host: any slug if HostWildcard, else exact match.
//
// Used by the routing resolver to decide whether a candidate
// HostBinding is allowed by the caller's Policy.
func (r Ref) Matches(providerSlug, modelSlug, hostSlug string) bool {
	if r.Provider != providerSlug {
		return false
	}
	if !r.ModelWildcard && r.Model != modelSlug {
		return false
	}
	if !r.HostWildcard && r.Host != hostSlug {
		return false
	}
	return true
}
