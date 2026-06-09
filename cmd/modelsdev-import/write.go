package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/manifest"
)

// WriteResult counts what was written.
type WriteResult struct {
	Providers, Hosts, Models, Bindings, Pricings int
	Preserved                                    int // existing provider/host files left untouched
	Errors                                       int
}

// overwriteHosts, when false (default), preserves existing provider.yaml and
// host.yaml files — those carry operator/hand-curated display + config
// (icons, baseURL, consoleURL) the importer must not clobber. Data files
// (models, bindings, pricing) are always (re)written.
var overwriteHosts bool

func sanitize(name string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(name)
}

// WriteToDisk emits the catalog-repo layout under root. Shipped entries go
// to the live tree; drafts go under root/drafts/ (skipped by manifest.LoadDir
// — invisible to both the SDK embed and the server seed until promoted).
func WriteToDisk(root string, r *TranslateResult) (*WriteResult, error) {
	wr := &WriteResult{}
	writeAll(filepath.Join(root), r.Providers, r.Hosts, r.Models, r.Bindings, r.Pricings, wr)
	d := r.Draft
	writeAll(filepath.Join(root, "drafts"), d.Providers, d.Hosts, d.Models, d.Bindings, d.Pricings, wr)
	return wr, nil
}

func writeAll(base string, provs []manifest.ProviderDTO, hosts []manifest.HostDTO, models []manifest.ModelDTO, binds []manifest.HostBindingDTO, prices []manifest.PricingDTO, wr *WriteResult) {
	for _, p := range provs {
		path := filepath.Join(base, "providers", p.Metadata.Name, "provider.yaml")
		if !overwriteHosts && fileExists(path) {
			wr.Preserved++
			continue
		}
		if writeYAML(path, p, wr) {
			wr.Providers++
		}
	}
	for _, h := range hosts {
		path := filepath.Join(base, "hosts", h.Metadata.Name, "host.yaml")
		if !overwriteHosts && fileExists(path) {
			wr.Preserved++
			continue
		}
		if writeYAML(path, h, wr) {
			wr.Hosts++
		}
	}
	for _, m := range models {
		owner := m.Metadata.Owner.Name
		if owner == "" {
			owner = m.Metadata.Owner.ID
		}
		path := filepath.Join(base, "providers", owner, "models", sanitize(m.Metadata.Name)+".yaml")
		if writeYAML(path, m, wr) {
			wr.Models++
		}
	}
	for _, b := range binds {
		path := filepath.Join(base, "hosts", b.Spec.Host, "bindings", sanitize(b.Metadata.Name)+".yaml")
		if writeYAML(path, b, wr) {
			wr.Bindings++
		}
	}
	for _, p := range prices {
		host := p.Metadata.Owner.ID
		base := filepath.Join(base, "hosts", host, "pricing")
		// strip the "<host>-" filename prefix; the host dir already scopes it.
		fname := strings.TrimPrefix(p.Metadata.Name, host+"-")
		path := filepath.Join(base, sanitize(fname)+".yaml")
		if writeYAML(path, p, wr) {
			wr.Pricings++
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeYAML(path string, v any, wr *WriteResult) bool {
	b, err := yaml.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal %s: %v\n", path, err)
		wr.Errors++
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", path, err)
		wr.Errors++
		return false
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
		wr.Errors++
		return false
	}
	return true
}
