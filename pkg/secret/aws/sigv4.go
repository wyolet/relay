package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const algorithm = "AWS4-HMAC-SHA256"

// signRequest adds SigV4 Authorization and required AWS headers to req. body is
// the exact payload bytes used for the x-amz-content-sha256 hash.
func signRequest(req *http.Request, body []byte, creds Credentials, region, service string, now time.Time) error {
	if req.URL == nil {
		return fmt.Errorf("secret/aws: request URL is nil")
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	req.Header.Set("Host", host)

	signed := signedHeaderNames(req.Header)
	canonical := buildCanonicalRequest(req.Method, req.URL.Path, req.URL.RawQuery, req.Header, signed, payloadHash)
	canonicalHash := sha256Hex([]byte(canonical))

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		canonicalHash,
	}, "\n")

	signingKey := deriveSigningKey(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm,
		creds.AccessKeyID,
		scope,
		strings.Join(signed, ";"),
		signature,
	))
	return nil
}

// buildCanonicalRequest assembles the SigV4 canonical request string.
func buildCanonicalRequest(method, uri, query string, hdr http.Header, signedNames []string, payloadHash string) string {
	if uri == "" {
		uri = "/"
	}
	var hdrLines []string
	for _, name := range signedNames {
		val := strings.TrimSpace(hdr.Get(name))
		hdrLines = append(hdrLines, strings.ToLower(name)+":"+val)
	}
	return strings.Join([]string{
		method,
		uri,
		query,
		strings.Join(hdrLines, "\n") + "\n",
		strings.Join(signedNames, ";"),
		payloadHash,
	}, "\n")
}

func signedHeaderNames(hdr http.Header) []string {
	seen := make(map[string]struct{})
	for k := range hdr {
		seen[strings.ToLower(k)] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
