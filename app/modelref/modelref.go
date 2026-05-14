// Package modelref parses the catalog reference DSL used by Policy
// grants and the /catalog/resolve admin endpoint.
//
// Grammar — five shapes, no wildcards in the wire form. Absence of a
// segment is the wildcard:
//
//	ref ::= provider                         "anthropic"                          all anthropic models, all hosts
//	      | provider "@" host                "anthropic@bedrock"                  all anthropic models, only on bedrock
//	      | provider "/" model               "anthropic/claude-opus-4-7"          this model on any host
//	      | provider "/" model "@" host      "anthropic/claude-opus-4-7@bedrock"  this exact binding only
//	      | "@" host                         "@bedrock"                           every model on this host
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
	Raw string
	// Provider is the provider slug. "" when ProviderWildcard is true
	// (host-only refs of the form "@host").
	Provider string
	// Model is the model slug, "" when ModelWildcard is true.
	Model string
	// Host is the host slug, "" when HostWildcard is true.
	Host string
	// ProviderWildcard is true for host-only refs ("@bedrock"). False
	// for every other shape.
	ProviderWildcard bool
	// ModelWildcard is true when the model position is "*" or absent.
	ModelWildcard bool
	// HostWildcard is true when the host position is "*" or absent.
	HostWildcard bool
}

// Kind classifies a Ref for the resolve API response.
type Kind string

const (
	KindProvider       Kind = "provider"         // provider                       — all models, all hosts
	KindProviderOnHost Kind = "provider-on-host" // provider@host                  — all models, single host
	KindModel          Kind = "model"            // provider/model                 — single model, all hosts
	KindBinding        Kind = "binding"          // provider/model@host            — single binding
	KindHost           Kind = "host"             // @host                          — all providers, all models, single host
)

// Kind returns the classification of this Ref. Five cases driven by
// the three wildcard bits.
func (r Ref) Kind() Kind {
	if r.ProviderWildcard {
		return KindHost
	}
	if r.ModelWildcard {
		if r.HostWildcard {
			return KindProvider
		}
		return KindProviderOnHost
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

	// Host-only refs ("@bedrock") — every model from every provider on
	// this host. Distinct from every other shape, which is provider-
	// anchored.
	if strings.HasPrefix(s, "@") {
		host := s[1:]
		if host == "" {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "trailing @ requires a host slug"}
		}
		if !slugRe.MatchString(host) {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "host must be a DNS-1123 slug"}
		}
		ref.ProviderWildcard = true
		ref.ModelWildcard = true
		ref.Host = host
		return ref, nil
	}

	// Optional @host suffix.
	hostPart := ""
	if i := strings.LastIndex(s, "@"); i >= 0 {
		hostPart = s[i+1:]
		s = s[:i]
		if hostPart == "" {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "trailing @ requires a host slug"}
		}
	}

	// provider[/model]
	slash := strings.IndexByte(s, '/')
	switch {
	case slash < 0:
		// provider only — model wildcarded by absence.
		ref.Provider = s
		ref.ModelWildcard = true
	case slash == 0:
		return ref, &SyntaxError{Raw: ref.Raw, Reason: "leading / — provider is required"}
	default:
		ref.Provider = s[:slash]
		ref.Model = s[slash+1:]
		if ref.Model == "" {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "trailing / requires a model slug"}
		}
	}

	if !slugRe.MatchString(ref.Provider) {
		return ref, &SyntaxError{Raw: ref.Raw, Reason: "provider must be a DNS-1123 slug"}
	}
	if !ref.ModelWildcard && !slugRe.MatchString(ref.Model) {
		return ref, &SyntaxError{Raw: ref.Raw, Reason: "model must be a DNS-1123 slug"}
	}

	// @host: present → concrete host; absent → host wildcard.
	if hostPart != "" {
		if !slugRe.MatchString(hostPart) {
			return ref, &SyntaxError{Raw: ref.Raw, Reason: "host must be a DNS-1123 slug"}
		}
		ref.Host = hostPart
	} else {
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
// binding triple. All three inputs are catalog slugs (Meta.Name). Each
// wildcard bit independently relaxes the corresponding equality check.
//
// Used by the routing resolver to decide whether a candidate
// HostBinding is allowed by the caller's Policy.
func (r Ref) Matches(providerSlug, modelSlug, hostSlug string) bool {
	if !r.ProviderWildcard && r.Provider != providerSlug {
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
