// icon-import downloads provider icons from OpenRouter's
// events/all-providers.json into a target directory (typically the
// relay-ui repo's public/provider/ folder).
//
// Usage:
//
//	go run ./cmd/icon-import -src ./events/all-providers.json -dst ../relay-ui/public/provider
//
// File naming: <slug>.<ext>. Extension is inferred from the upstream
// Content-Type when possible, falling back to the URL path's suffix,
// then to .ico.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type providersFile struct {
	Data []provider `json:"data"`
}

type provider struct {
	Slug string `json:"slug"`
	Icon *struct {
		URL string `json:"url"`
	} `json:"icon"`
}

func main() {
	src := flag.String("src", "events/all-providers.json", "path to OpenRouter all-providers.json")
	dst := flag.String("dst", "../relay-ui/public/provider", "output directory")
	timeout := flag.Duration("timeout", 15*time.Second, "per-icon download timeout")
	overwrite := flag.Bool("overwrite", false, "redownload icons even if a file with the slug already exists")
	flag.Parse()

	b, err := os.ReadFile(*src)
	if err != nil {
		log.Fatalf("read %s: %v", *src, err)
	}
	var pf providersFile
	if err := json.Unmarshal(b, &pf); err != nil {
		log.Fatalf("parse: %v", err)
	}
	if err := os.MkdirAll(*dst, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *dst, err)
	}

	client := &http.Client{Timeout: *timeout}
	var ok, skipped, failed int
	for _, p := range pf.Data {
		if p.Slug == "" || p.Icon == nil || p.Icon.URL == "" {
			skipped++
			continue
		}
		existing, _ := filepath.Glob(filepath.Join(*dst, p.Slug+".*"))
		if len(existing) > 0 && !*overwrite {
			fmt.Printf("skip   %s (already %s)\n", p.Slug, filepath.Base(existing[0]))
			skipped++
			continue
		}
		ext, body, err := fetchIcon(client, absURL(p.Icon.URL))
		if err != nil {
			fmt.Printf("FAIL   %s: %v\n", p.Slug, err)
			failed++
			continue
		}
		out := filepath.Join(*dst, p.Slug+ext)
		if err := os.WriteFile(out, body, 0o644); err != nil {
			fmt.Printf("FAIL   %s: write: %v\n", p.Slug, err)
			failed++
			continue
		}
		fmt.Printf("ok     %s -> %s (%d bytes)\n", p.Slug, filepath.Base(out), len(body))
		ok++
	}
	fmt.Printf("\n%d ok, %d skipped, %d failed\n", ok, skipped, failed)
	// Don't exit 1 on partial failures — typical causes are upstream
	// 404s on providers whose icon link rotted. Operator fills the
	// remaining slugs from simple-icons or the provider's own brand kit.
}

// absURL resolves OpenRouter-relative icon paths (e.g. "/images/icons/
// Anthropic.svg") against openrouter.ai. Absolute URLs pass through.
func absURL(u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if strings.HasPrefix(u, "/") {
		return "https://openrouter.ai" + u
	}
	return u
}

func fetchIcon(c *http.Client, url string) (ext string, body []byte, err error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	ext = extFromContentType(resp.Header.Get("Content-Type"))
	if ext == "" {
		ext = extFromURLPath(url)
	}
	if ext == "" {
		ext = ".ico"
	}
	return ext, body, nil
}

func extFromContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	switch mt {
	case "image/svg+xml":
		return ".svg"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return ".ico"
	case "image/gif":
		return ".gif"
	}
	return ""
}

func extFromURLPath(rawURL string) string {
	// Strip query so faviconV2-style URLs don't confuse the suffix.
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rawURL = rawURL[:i]
	}
	switch strings.ToLower(path.Ext(rawURL)) {
	case ".svg":
		return ".svg"
	case ".png":
		return ".png"
	case ".jpg", ".jpeg":
		return ".jpg"
	case ".webp":
		return ".webp"
	case ".ico":
		return ".ico"
	case ".gif":
		return ".gif"
	}
	return ""
}
