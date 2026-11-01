// Package model is the domain layer for the Model entity — a container
// for a family of dated checkpoints (Snapshots). Meta.Name is the admin /
// catalog slug; customers address the model by sending a Snapshot.Name in
// the request body. Snapshot.Pointer names the default snapshot displayed
// in /v1/models.
//
// Host serving info (which Hosts can serve this Model) lives in standalone
// HostBinding entities (app/binding), not embedded in the Model spec.
//
// Pricing and RateLimit attachments are deferred until those packages land.
package model

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/slug"
)

// Model is the published model definition.
type Model struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is the body. Cross-refs are stored as ids. The owning Provider
// (vendor) id lives on Meta.Owner.ID (Owner.Kind is always "provider");
// there is no separate spec.providerId field.
//
// Per-Host serving info (which Hosts can serve this Model) lives in
// standalone HostBinding entities (app/binding), not in Spec.
type Spec struct {
	Family string `json:"family,omitempty" yaml:"family,omitempty" doc:"Model family, such as claude or gpt."`
	// ProductLine is the vendor's named tier or variant inside Family (opus, flash-lite); never a version or size. Allowed values are checked by the catalog validator, not here.
	ProductLine string `json:"productLine,omitempty" yaml:"productLine,omitempty" doc:"The vendor's named variant inside the family, such as opus or flash-lite; never a version or size."`
	Version     string `json:"version,omitempty"     yaml:"version,omitempty"     doc:"Version label inside the product line."`

	Capabilities Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty" doc:"What the model supports."`
	Modalities   Modalities   `json:"modalities,omitempty"   yaml:"modalities,omitempty"   doc:"Input and output modalities."`

	// Context window split. ContextWindowTotal is canonical; the Input/Output
	// pair is informational for models that publish a soft cap.
	ContextWindowInput  int `json:"contextWindowInput,omitempty"  yaml:"contextWindowInput,omitempty"  validate:"omitempty,gte=0" doc:"Published input cap in tokens, for models that have one. Informational."`
	ContextWindowOutput int `json:"contextWindowOutput,omitempty" yaml:"contextWindowOutput,omitempty" validate:"omitempty,gte=0" doc:"Published output share of the context window in tokens. Informational."`
	ContextWindowTotal  int `json:"contextWindowTotal,omitempty"  yaml:"contextWindowTotal,omitempty"  validate:"omitempty,gte=0" doc:"Context window in tokens."`
	MaxOutputTokens     int `json:"maxOutputTokens,omitempty"     yaml:"maxOutputTokens,omitempty"     validate:"omitempty,gte=0" doc:"Most tokens one response may generate."`

	KnowledgeCutoff string       `json:"knowledgeCutoff,omitempty" yaml:"knowledgeCutoff,omitempty" doc:"Training-data cutoff, YYYY-MM or YYYY-MM-DD."`
	ReleaseDate     string       `json:"releaseDate,omitempty"     yaml:"releaseDate,omitempty"     doc:"Release date, YYYY-MM-DD."`
	DeprecationDate string       `json:"deprecationDate,omitempty" yaml:"deprecationDate,omitempty" doc:"Date the vendor deprecated the model, YYYY-MM-DD."`
	Deprecation     *Deprecation `json:"deprecation,omitempty"     yaml:"deprecation,omitempty"     doc:"Lifecycle state. Absent means active."`

	Tags                 []string `json:"tags,omitempty"                 yaml:"tags,omitempty"                 doc:"Free-form tags; list endpoints filter on them with ?tag=."`
	Documentation        string   `json:"documentation,omitempty"        yaml:"documentation,omitempty"        doc:"Link to the model's documentation."`
	License              string   `json:"license,omitempty"              yaml:"license,omitempty"              doc:"License the weights are published under, for open models."`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty" yaml:"providerModelPageURL,omitempty" validate:"omitempty,http_url" doc:"The provider's page for the model."`

	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False stops routing to the model (403 model_disabled). Omitted means true."` // nil = true

	// Snapshots are the dated checkpoints this Model exposes. Every Model has
	// at least one. Pointer names the snapshot the bare Model name resolves
	// to at request time.
	Snapshots []Snapshot `json:"snapshots" yaml:"snapshots" validate:"required,min=1,dive" doc:"Dated checkpoints of the model; at least one."`
	Pointer   string     `json:"pointer"   yaml:"pointer"   validate:"required" doc:"Name of the snapshot the bare model name resolves to."`

	// Aliases are resolution-only, last-priority matchers. A request whose
	// model string matches no catalog name (snapshot names + their
	// synthesized provider/host forms) is checked against declared aliases:
	// exact forms first, then patterns — the ONLY place wildcards are
	// allowed (exactly one '*', with a literal prefix). A match resolves to
	// this Model's Pointer snapshot and the caller's string goes upstream
	// VERBATIM. Aliases are lookup keys, never identity: they never appear
	// in policy grants, modelref, admin URLs, or the usage `model` slug.
	// Matching runs on the slug-normalized form ("claude-fable-5[1m]" ≡
	// "claude-fable-5-1m").
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty" doc:"Extra request names that resolve to this model's pointer snapshot when no catalog name matches; the caller's string goes upstream unchanged. One * wildcard with a literal prefix is allowed, such as claude-opus-5[*]. Never usable in policies."`
}

// Snapshot is a dated checkpoint of a Model. Name is the customer-facing
// identifier (DNS-1123 slug; matches what the SDK sends in the `model`
// field). OriginalName is the upstream wire name; when omitted it
// defaults to Name — the common case where the slug-safe name already
// matches what the provider expects.
type Snapshot struct {
	Name         string `json:"name"                   yaml:"name"                   validate:"required,hostname_rfc1123" doc:"Snapshot slug, usable as a model name in requests."`
	ReleasedAt   string `json:"releasedAt,omitempty"   yaml:"releasedAt,omitempty"   doc:"Snapshot release date, YYYY-MM-DD."`
	OriginalName string `json:"originalName,omitempty" yaml:"originalName,omitempty" doc:"The vendor's own name for the snapshot when it differs from name."`
}

// Upstream returns the wire name to send to the provider — OriginalName
// when set, otherwise Name.
func (s Snapshot) Upstream() string {
	if s.OriginalName != "" {
		return s.OriginalName
	}
	return s.Name
}

// PointerUpstream returns the upstream wire name of the snapshot the bare
// model name resolves to (Pointer → snapshot → Upstream()). Falls back to
// Pointer itself if no snapshot matches (shouldn't happen post-Validate).
func (m *Model) PointerUpstream() string {
	if s := m.PointerSnapshot(); s != nil {
		return s.Upstream()
	}
	return m.Spec.Pointer
}

// PointerSnapshot returns the snapshot Pointer names, nil when none
// matches (can't happen post-Validate; callers stay defensive).
func (m *Model) PointerSnapshot() *Snapshot {
	for i := range m.Spec.Snapshots {
		if lower(m.Spec.Snapshots[i].Name) == lower(m.Spec.Pointer) {
			return &m.Spec.Snapshots[i]
		}
	}
	return nil
}

// AliasPattern reports whether alias is a wildcard pattern and, when it
// is, returns the slug-normalized literal prefix and suffix used for
// boundary-accurate matching against From-normalized lookup keys.
func AliasPattern(alias string) (prefix, suffix string, ok bool) {
	pre, post, found := strings.Cut(alias, "*")
	if !found {
		return "", "", false
	}
	return slug.FromPrefix(pre), slug.FromSuffix(post), true
}

// Capabilities is the bag-of-bools feature flags every model declares.
// Open-ended over time; new flags can be added without a migration since
// the whole struct lives in JSONB.
type Capabilities struct {
	// UnsupportedParams lists request parameters (canonical sampling names:
	// "temperature", "top_p", "top_k") the upstream rejects for this model.
	// Inverted so the zero value means "everything supported" — only flagged
	// models carry it. The pipeline strips listed params pre-upstream and
	// surfaces the drop; unknown names are inert, so catalog data may flag
	// params an older relay doesn't yet gate.
	UnsupportedParams []string `json:"unsupportedParams,omitempty" yaml:"unsupportedParams,omitempty" doc:"Request parameters (canonical sampling names such as temperature, top_p, top_k) the upstream rejects for this model; relay strips them before sending and reports the drop."`
	Chat              bool     `json:"chat,omitempty"              yaml:"chat,omitempty"              doc:"Generates chat or text responses."`
	Embeddings        bool     `json:"embeddings,omitempty"        yaml:"embeddings,omitempty"        doc:"Produces embeddings."`
	Streaming         bool     `json:"streaming,omitempty"         yaml:"streaming,omitempty"         doc:"Streams responses."`
	Tools             bool     `json:"tools,omitempty"             yaml:"tools,omitempty"             doc:"Calls tools."`
	ParallelTools     bool     `json:"parallelTools,omitempty"     yaml:"parallelTools,omitempty"     doc:"Calls several tools in one turn."`
	Vision            bool     `json:"vision,omitempty"            yaml:"vision,omitempty"            doc:"Accepts image input."`
	Audio             bool     `json:"audio,omitempty"             yaml:"audio,omitempty"             doc:"Handles audio, in either direction."`
	PromptCache       bool     `json:"promptCache,omitempty"       yaml:"promptCache,omitempty"       doc:"Supports prompt caching."`
	Reasoning         bool     `json:"reasoning,omitempty"         yaml:"reasoning,omitempty"         doc:"Produces reasoning before answering."`
	JSONMode          bool     `json:"jsonMode,omitempty"          yaml:"jsonMode,omitempty"          doc:"Supports JSON output mode."`
	StructuredOutputs bool     `json:"structuredOutputs,omitempty" yaml:"structuredOutputs,omitempty" doc:"Follows a supplied JSON schema for its output."`
	Batch             bool     `json:"batch,omitempty"             yaml:"batch,omitempty"             doc:"Available through the provider's batch API."`
	ComputerUse       bool     `json:"computerUse,omitempty"       yaml:"computerUse,omitempty"       doc:"Supports computer-use tools."`
	WebSearch         bool     `json:"webSearch,omitempty"         yaml:"webSearch,omitempty"         doc:"Supports the provider's web search tool."`
	FileInput         bool     `json:"fileInput,omitempty"         yaml:"fileInput,omitempty"         doc:"Accepts file input, such as PDFs."`
	AudioInput        bool     `json:"audioInput,omitempty"        yaml:"audioInput,omitempty"        doc:"Accepts audio input."`
	AudioOutput       bool     `json:"audioOutput,omitempty"       yaml:"audioOutput,omitempty"       doc:"Produces audio output."`
	SystemMessages    bool     `json:"systemMessages,omitempty"    yaml:"systemMessages,omitempty"    doc:"Accepts system instructions."`
	AssistantPrefill  bool     `json:"assistantPrefill,omitempty"  yaml:"assistantPrefill,omitempty"  doc:"Continues from a prefilled assistant message."`

	// ReasoningEfforts lists the effort levels the model accepts, ordered
	// low→high, from ReasoningEffortLevels. Empty means the model has no
	// effort control. Both fields require Reasoning.
	ReasoningEfforts       []string `json:"reasoningEfforts,omitempty"       yaml:"reasoningEfforts,omitempty"       enum:"none,minimal,low,medium,high,xhigh,max" doc:"Reasoning effort levels the model accepts, low to high. Empty means no effort control. Needs reasoning."`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty" yaml:"defaultReasoningEffort,omitempty" enum:"none,minimal,low,medium,high,xhigh,max" doc:"Effort level used when a request names none; one of reasoningEfforts."`
}

// ReasoningEffortLevels is the canonical effort set, ordered low→high.
var ReasoningEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Modalities lists input/output media types ("text", "image", "audio", ...).
type Modalities struct {
	Input  []string `json:"input,omitempty"  yaml:"input,omitempty"  doc:"Media the model accepts, such as text, image, audio, video, pdf."`
	Output []string `json:"output,omitempty" yaml:"output,omitempty" doc:"Media the model produces."`
}

// Deprecation describes the lifecycle state of a Model.
type Deprecation struct {
	Status      DeprecationStatus `json:"status,omitempty"      yaml:"status,omitempty"      validate:"omitempty,oneof=active deprecated sunset" enum:"active,deprecated,sunset" doc:"active, deprecated (still served, left out of policy wildcards) or sunset (retired by the vendor)."`
	SunsetDate  string            `json:"sunsetDate,omitempty"  yaml:"sunsetDate,omitempty"  doc:"Date the vendor stops serving the model, YYYY-MM-DD."`
	Replacement string            `json:"replacement,omitempty" yaml:"replacement,omitempty" validate:"omitempty,uuid" doc:"Id of the model that replaces this one."`
}

// DeprecationStatus enumerates the lifecycle states.
type DeprecationStatus string

const (
	DeprecationActive     DeprecationStatus = "active"
	DeprecationDeprecated DeprecationStatus = "deprecated"
	DeprecationSunset     DeprecationStatus = "sunset"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (m *Model) IsEnabled() bool { return m.Spec.Enabled == nil || *m.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Model-specific invariants:
//   - Owner is required and must be provider-kind with a non-empty ID.
//     Models always belong to a Provider.
//   - Snapshot names are unique within the model and Pointer must name
//     one of them.
//   - Reasoning effort levels are known, unique, ordered low→high, and
//     only set on reasoning models; the default is one of them.
//
// Cross-entity checks (Owner.ID resolves to a real Provider; Deprecation.
// Replacement resolves to a real Model; snapshot-name uniqueness across
// the catalog) live in the composition layer.
func (m *Model) Validate() error {
	if err := meta.Validator.Struct(m); err != nil {
		return err
	}
	if m.Meta.Owner.Kind != meta.OwnerProvider {
		return fmt.Errorf("model %q: owner.kind must be provider, got %q", m.Meta.Name, m.Meta.Owner.Kind)
	}
	if m.Meta.Owner.ID == "" {
		return fmt.Errorf("model %q: owner.id is required (provider id)", m.Meta.Name)
	}
	snaps := make(map[string]struct{}, len(m.Spec.Snapshots))
	snapSlugs := make(map[string]struct{}, len(m.Spec.Snapshots))
	for _, s := range m.Spec.Snapshots {
		key := lower(s.Name)
		if _, dup := snaps[key]; dup {
			return fmt.Errorf("model %q: duplicate snapshot %q", m.Meta.Name, s.Name)
		}
		snaps[key] = struct{}{}
		snapSlugs[slug.From(s.Name)] = struct{}{}
	}
	if _, ok := snaps[lower(m.Spec.Pointer)]; !ok {
		return fmt.Errorf("model %q: pointer %q does not match any snapshot", m.Meta.Name, m.Spec.Pointer)
	}
	if err := m.validateReasoningEfforts(); err != nil {
		return err
	}
	return m.validateAliases(snapSlugs)
}

// validateReasoningEfforts enforces that the effort list holds known levels
// in strictly ascending canonical order, the default is one of them, and
// neither is set on a model without reasoning.
func (m *Model) validateReasoningEfforts() error {
	c := m.Spec.Capabilities
	if len(c.ReasoningEfforts) == 0 && c.DefaultReasoningEffort == "" {
		return nil
	}
	if !c.Reasoning {
		return fmt.Errorf("model %q: reasoningEfforts/defaultReasoningEffort require capabilities.reasoning", m.Meta.Name)
	}
	prev := -1
	seen := make(map[string]struct{}, len(c.ReasoningEfforts))
	for _, e := range c.ReasoningEfforts {
		rank := slices.Index(ReasoningEffortLevels, e)
		if rank < 0 {
			return fmt.Errorf("model %q: unknown reasoning effort %q (want one of %s)", m.Meta.Name, e, strings.Join(ReasoningEffortLevels, ", "))
		}
		if _, dup := seen[e]; dup {
			return fmt.Errorf("model %q: duplicate reasoning effort %q", m.Meta.Name, e)
		}
		seen[e] = struct{}{}
		if rank < prev {
			return fmt.Errorf("model %q: reasoning effort %q out of order — reasoningEfforts must be ordered low to high", m.Meta.Name, e)
		}
		prev = rank
	}
	if c.DefaultReasoningEffort != "" {
		if _, ok := seen[c.DefaultReasoningEffort]; !ok {
			return fmt.Errorf("model %q: defaultReasoningEffort %q is not in reasoningEfforts", m.Meta.Name, c.DefaultReasoningEffort)
		}
	}
	return nil
}

// validateAliases enforces the alias rules: at most one '*', patterns
// need a usable literal prefix, no duplicates on the normalized form
// (which also catches MaxLen-truncation collisions), and an exact alias
// may not shadow one of the model's own snapshot names — exact catalog
// names always win at resolution, so such an alias would be dead.
func (m *Model) validateAliases(snapSlugs map[string]struct{}) error {
	seen := make(map[string]struct{}, len(m.Spec.Aliases))
	for _, a := range m.Spec.Aliases {
		if a == "" {
			return fmt.Errorf("model %q: empty alias", m.Meta.Name)
		}
		if strings.Count(a, "*") > 1 {
			return fmt.Errorf("model %q: alias %q: at most one '*' is allowed", m.Meta.Name, a)
		}
		var key string
		if prefix, suffix, isPattern := AliasPattern(a); isPattern {
			if a[0] == '*' {
				return fmt.Errorf("model %q: alias %q: pattern must start with a literal prefix", m.Meta.Name, a)
			}
			if prefix == "" {
				return fmt.Errorf("model %q: alias %q: pattern prefix has no usable characters", m.Meta.Name, a)
			}
			key = prefix + "*" + suffix
		} else {
			key = slug.From(a)
			if key == "" {
				return fmt.Errorf("model %q: alias %q has no usable characters", m.Meta.Name, a)
			}
			if _, shadows := snapSlugs[key]; shadows {
				return fmt.Errorf("model %q: alias %q normalizes to snapshot name %q — exact names always win, the alias would be dead", m.Meta.Name, a, key)
			}
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("model %q: duplicate alias %q (normalized form %q)", m.Meta.Name, a, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// lower is a tiny ascii-lowercase to avoid importing strings just for one use.
func lower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
