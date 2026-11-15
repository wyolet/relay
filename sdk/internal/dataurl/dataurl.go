// Package dataurl reads RFC 2397 data: URLs for the vendor adapters whose
// wire takes inline media only as a media type plus base64 data. It does not
// build data URLs or check that a ;base64 payload is valid base64; the
// upstream judges the bytes.
package dataurl

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// Base64 returns the media type (parameters dropped, empty when the URL names
// none) and the base64 payload of a data: URL. A payload without ;base64 is
// percent-decoded and re-encoded as base64.
func Base64(dataURL string) (mediaType, data string, err error) {
	rest, ok := strings.CutPrefix(dataURL, "data:")
	if !ok {
		return "", "", errors.New("not a data: URL")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", errors.New("data: URL has no comma before its payload")
	}
	meta, isBase64 := strings.CutSuffix(meta, ";base64")
	mediaType, _, _ = strings.Cut(meta, ";")
	if isBase64 {
		return mediaType, payload, nil
	}
	raw, err := url.PathUnescape(payload)
	if err != nil {
		return "", "", err
	}
	return mediaType, base64.StdEncoding.EncodeToString([]byte(raw)), nil
}
