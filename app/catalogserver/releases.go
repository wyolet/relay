package catalogserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxSumBytes   = 4 << 10
	maxAssetBytes = 64 << 20
	releaseTime   = time.Minute
)

var errNotPublished = errors.New("not published")

// releases reads the files published with each tag under <base>/<tag>/<name>.
type releases struct {
	base   string
	client *http.Client
}

// get follows redirects (GitHub serves release files from another host). A
// 404 is errNotPublished.
func (r releases) get(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseTime)
	defer cancel()
	url := strings.TrimRight(r.base, "/") + "/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errNotPublished
	default:
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return body, nil
}
