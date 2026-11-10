package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is one parsed document: exactly one catalog-kind field is set, or Foreign, or Ext.
type Document struct {
	Provider    *ProviderDTO
	Host        *HostDTO
	Model       *ModelDTO
	Policy      *PolicyDTO
	RateLimit   *RateLimitDTO
	Pricing     *PricingDTO
	HostBinding *HostBindingDTO

	// Foreign is a kind owned by a sibling subsystem sharing the tree; LoadDir drops it.
	Foreign string

	// Ext is a document of a kind registered beyond the catalog's; its decoder chose the type.
	Ext any
}

// Envelope is a document with its spec still undecoded.
type Envelope struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   WireMeta  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

// KindDecoder is how a Parser reads one kind.
type KindDecoder struct {
	Decode func(env *Envelope) (Document, error)
	// RemovedFields maps spec fields the kind no longer has to what replaces each. The lenient decode would ignore them, and a dropped restriction widens access, so a document carrying one is refused.
	RemovedFields map[string]string
	// ReshapedHint, when set, refuses the kind under PreviousAPIVersion with this pointer to the new shape.
	ReshapedHint string
}

// InterceptFunc sees a document before the apiVersion check. handled reports that it decoded or refused the document, which then skips the registered kinds.
type InterceptFunc func(index int, env *Envelope, node *yaml.Node) (doc Document, handled bool, err error)

// Parser decodes the kinds registered on it. It is safe for concurrent use once registration is done.
type Parser struct {
	kinds     map[string]KindDecoder
	intercept InterceptFunc
}

// NewParser returns a Parser with the catalog kinds registered.
func NewParser() *Parser {
	p := &Parser{kinds: map[string]KindDecoder{}}
	registerCatalogKinds(p)
	return p
}

// Register adds a kind. Registering a kind twice is a programming error and panics.
func (p *Parser) Register(kind string, k KindDecoder) {
	if _, dup := p.kinds[kind]; dup {
		panic("manifest: kind " + kind + " registered twice")
	}
	p.kinds[kind] = k
}

// Intercept sets f to see every document before the apiVersion check.
func (p *Parser) Intercept(f InterceptFunc) { p.intercept = f }

var catalogParser = NewParser()

// Parse reads the catalog kinds from r; see Parser.Parse.
func Parse(r io.Reader) ([]Document, error) { return catalogParser.Parse(r) }

// LoadDir reads the catalog kinds under dir; see Parser.LoadDir.
func LoadDir(dir string) ([]Document, error) { return catalogParser.LoadDir(dir) }

// Parse reads one or more YAML documents from r. Multi-doc YAML (---
// separated) is fully supported; empty documents are skipped.
func (p *Parser) Parse(r io.Reader) ([]Document, error) {
	dec := yaml.NewDecoder(r)
	var docs []Document
	docIdx := 0
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		var env Envelope
		if err == nil {
			err = node.Decode(&env)
		}
		if err != nil {
			return nil, fmt.Errorf("wire: doc %d: %w", docIdx, err)
		}
		if env.Kind == "" && env.APIVersion == "" {
			docIdx++
			continue
		}
		if p.intercept != nil {
			doc, handled, err := p.intercept(docIdx, &env, &node)
			if err != nil {
				return nil, fmt.Errorf("wire: %w", err)
			}
			if handled {
				docs = append(docs, doc)
				docIdx++
				continue
			}
		}
		if err := p.checkAPIVersion(env.APIVersion, env.Kind); err != nil {
			return nil, fmt.Errorf("wire: doc %d: %w", docIdx, err)
		}
		if env.Metadata.Name == "" {
			return nil, fmt.Errorf("wire: doc %d kind=%s: metadata.name required", docIdx, env.Kind)
		}
		doc, err := p.decode(&env)
		if err != nil {
			return nil, fmt.Errorf("wire: doc %d kind=%s name=%s: %w", docIdx, env.Kind, env.Metadata.Name, err)
		}
		docs = append(docs, doc)
		docIdx++
	}
	return docs, nil
}

func (p *Parser) decode(env *Envelope) (Document, error) {
	k, ok := p.kinds[env.Kind]
	if !ok {
		return Document{}, fmt.Errorf("unknown kind %q", env.Kind)
	}
	if err := refuseRemovedFields(env.Kind, k.RemovedFields, &env.Spec); err != nil {
		return Document{}, err
	}
	return k.Decode(env)
}

func refuseRemovedFields(kind string, removed map[string]string, spec *yaml.Node) error {
	if len(removed) == 0 || spec.Kind != yaml.MappingNode {
		return nil
	}
	var found []string
	for i := 0; i+1 < len(spec.Content); i += 2 {
		k := spec.Content[i].Value
		if hint, ok := removed[k]; ok {
			found = append(found, fmt.Sprintf("spec.%s (use %s)", k, hint))
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("%s no longer has %s", kind, strings.Join(found, ", "))
}

// LoadDir walks dir for .yaml / .yml files and parses each. Documents from
// all files are merged into a single slice. File order is deterministic
// (lexicographic).
//
// Any directory named "drafts" at any depth is skipped, as is any file
// matching "*.draft.yaml" / "*.draft.yml". The catalog uses these to gate
// resources that have been imported but not yet curated (e.g. unverified
// baseURL/adapter). Promote by moving the subtree out of drafts/ or by
// dropping the .draft suffix.
func (p *Parser) LoadDir(dir string) ([]Document, error) {
	var all []Document
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "drafts" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := filepath.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if strings.HasSuffix(name, ".draft"+ext) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("wire: open %s: %w", path, err)
		}
		defer f.Close()
		docs, err := p.Parse(f)
		if err != nil {
			return fmt.Errorf("wire: %s: %w", path, err)
		}
		for _, doc := range docs {
			if doc.Foreign == "" {
				all = append(all, doc)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}
