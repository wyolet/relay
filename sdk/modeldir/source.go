package modeldir

import (
	"net/http"

	"github.com/wyolet/relay/sdk/catalogsource"
)

// Source yields a full catalog that Add and Refresh pick models from. It is never merged into Load.
type Source = catalogsource.Source

// ReleaseOption configures Release.
type ReleaseOption = catalogsource.ReleaseOption

const (
	// Channel is catalogsource.Channel.
	Channel = catalogsource.Channel
	// DefaultIndexURL is catalogsource.DefaultIndexURL.
	DefaultIndexURL = catalogsource.DefaultIndexURL
	// DefaultDownloadURL is catalogsource.DefaultDownloadURL.
	DefaultDownloadURL = catalogsource.DefaultDownloadURL
)

// Embedded is catalogsource.Embedded.
func Embedded() Source { return catalogsource.Embedded() }

// File is catalogsource.File.
func File(path string) Source { return catalogsource.File(path) }

// Release is catalogsource.Release.
func Release(tag, cacheDir string, opts ...ReleaseOption) Source {
	return catalogsource.Release(tag, cacheDir, opts...)
}

// WithIndexURL is catalogsource.WithIndexURL.
func WithIndexURL(url string) ReleaseOption { return catalogsource.WithIndexURL(url) }

// WithDownloadURL is catalogsource.WithDownloadURL.
func WithDownloadURL(base string) ReleaseOption { return catalogsource.WithDownloadURL(base) }

// WithHTTPClient is catalogsource.WithHTTPClient.
func WithHTTPClient(c *http.Client) ReleaseOption { return catalogsource.WithHTTPClient(c) }
