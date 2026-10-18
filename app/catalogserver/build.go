package catalogserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/catalogembed"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/seed"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// CatalogAsset is the SDK catalog file of every release.
const CatalogAsset = "catalog.json.gz"

// DataAsset is the relay seed tree file of the release tagged tag.
func DataAsset(tag string) string { return "relay-catalog-data-" + tag + ".tar.gz" }

// BuildData builds the data tarball of commit. It never depends on compose,
// so a tree this binary can't load still yields its tarball.
func BuildData(ctx context.Context, repo Repo, commit string) ([]byte, error) {
	return repo.Archive(ctx, commit)
}

// BuildCatalog composes catalog.json.gz from a data tarball, so the catalog
// describes exactly the published tree. generatedAt is the tagged commit's
// time and the gzip header carries no name or mtime, which makes the bytes a
// function of the tree and this builder only.
func BuildCatalog(archive []byte, generatedAt time.Time, version string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "catalog-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := seed.ExtractCatalogArchive(bytes.NewReader(archive), dir); err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	dataDir := filepath.Join(dir, "data")
	docs, err := manifest.LoadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	cat, err := catalogembed.Compose(docs, generatedAt)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if err := catalogembed.ValidateAdapters(cat); err != nil {
		return nil, err
	}
	cat.Version = version
	data, err := catalogembed.MarshalJSON(cat)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	if _, err := sdkcatalog.LoadBytes(data); err != nil {
		return nil, fmt.Errorf("sdk load: %w", err)
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SumFile is data's checksum line in sha256sum format.
func SumFile(name string, data []byte) []byte {
	return []byte(sha256Hex(data) + "  " + name + "\n")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// parseSumFile returns the hex digest of a sha256sum line naming name.
func parseSumFile(b []byte, name string) (string, error) {
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f) > 2 {
		return "", errors.New("not a sha256sum line")
	}
	sum := strings.ToLower(f[0])
	if len(sum) != 64 || !isHexOID(sum) {
		return "", fmt.Errorf("invalid sha256 %q", f[0])
	}
	if len(f) == 2 && strings.TrimPrefix(f[1], "*") != name {
		return "", fmt.Errorf("names %q, want %q", f[1], name)
	}
	return sum, nil
}

var channelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// ArchiveChannel is the schema channel of a data tarball: the apiVersion
// suffix every document in it shares. It reads only apiVersion, so a schema
// this binary can't load still gets its channel.
func ArchiveChannel(archive []byte) (string, error) {
	dir, err := os.MkdirTemp("", "catalog-channel-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := seed.ExtractCatalogArchive(bytes.NewReader(archive), dir); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	apiVersion, err := treeAPIVersion(filepath.Join(dir, "data"))
	if err != nil {
		return "", err
	}
	channel := apiVersion[strings.LastIndex(apiVersion, "/")+1:]
	if !channelPattern.MatchString(channel) {
		return "", fmt.Errorf("apiVersion %q: invalid channel", apiVersion)
	}
	return channel, nil
}

// treeAPIVersion walks the files manifest.LoadDir would load.
func treeAPIVersion(dir string) (string, error) {
	var apiVersion, first string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if name == "drafts" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(name)
		if (ext != ".yaml" && ext != ".yml") || strings.HasSuffix(name, ".draft"+ext) {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		for i := 0; ; i++ {
			var doc struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
			}
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: doc %d: %w", rel, i, err)
			}
			switch {
			case doc.APIVersion == "" && doc.Kind == "":
			case doc.APIVersion == "":
				return fmt.Errorf("%s: doc %d: no apiVersion", rel, i)
			case apiVersion == "":
				apiVersion, first = doc.APIVersion, rel
			case doc.APIVersion != apiVersion:
				return fmt.Errorf("%s has apiVersion %q, %s has %q", rel, doc.APIVersion, first, apiVersion)
			}
		}
	})
	if err != nil {
		return "", err
	}
	if apiVersion == "" {
		return "", errors.New("no documents")
	}
	return apiVersion, nil
}
