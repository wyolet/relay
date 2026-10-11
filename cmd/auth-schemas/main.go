// Command auth-schemas writes one JSON Schema per auth-module manifest kind (Team, Project, ServiceAccount, Role, User, Group, RoleBinding) as relay registers them: relay's rule vocabulary and its own spec fields included. Run `make schemas` to regenerate.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/wyolet/relay/app/manifest"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

const schemaIDBase = "https://relay-api.wyolet.dev/schemas/"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: auth-schemas <out-dir>")
		os.Exit(2)
	}
	if err := write(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func write(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	all, err := manifest.AuthManifests.JSONSchemas()
	if err != nil {
		return err
	}
	for _, kind := range authmanifest.Kinds() {
		var doc map[string]any
		if err := json.Unmarshal(all[kind], &doc); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		doc["$id"] = schemaIDBase + path.Base(authmanifest.APIVersion) + "/" + kind + ".schema.json"
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		file := filepath.Join(out, kind+".schema.json")
		if err := os.WriteFile(file, append(body, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", file)
	}
	return nil
}
