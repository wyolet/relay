package aws

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// Vectors from AWS S3 SigV4 documentation (header-based auth examples).
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
const (
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func TestSigV4_GetObjectCanonicalRequest(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Host", "examplebucket.s3.amazonaws.com")
	hdr.Set("Range", "bytes=0-9")
	hdr.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	hdr.Set("X-Amz-Date", "20130524T000000Z")

	signed := []string{"host", "range", "x-amz-content-sha256", "x-amz-date"}
	payloadHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	got := buildCanonicalRequest("GET", "/test.txt", "", hdr, signed, payloadHash)
	want := "GET\n/test.txt\n\n" +
		"host:examplebucket.s3.amazonaws.com\n" +
		"range:bytes=0-9\n" +
		"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n" +
		"x-amz-date:20130524T000000Z\n\n" +
		"host;range;x-amz-content-sha256;x-amz-date\n" +
		payloadHash

	if got != want {
		t.Fatalf("canonical request mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}

	canonicalHash := sha256Hex([]byte(got))
	if canonicalHash != "7344ae5b7ee6c3e7e6b0fe0640412a37625d1fbfff95c48bbb2dc43964946972" {
		t.Fatalf("canonical hash: got %s", canonicalHash)
	}

	scope := "20130524/us-east-1/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n20130524T000000Z\n" + scope + "\n" + canonicalHash
	key := deriveSigningKey(testSecretKey, "20130524", "us-east-1", "s3")
	sig := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	if sig != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Fatalf("signature: got %s", sig)
	}
}

func TestSigV4_SignRequest_IAMListUsers(t *testing.T) {
	body := []byte("Action=ListUsers&Version=2011-06-15")
	req, err := http.NewRequest(http.MethodPost, "https://iam.amazonaws.com/", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	fixed := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	creds := Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey}
	if err := SignRequest(req, body, creds, "us-east-1", "iam", fixed); err != nil {
		t.Fatal(err)
	}

	// The string to sign, written out by hand from the SigV4 rules rather than through the helpers under test.
	const bodyHash = "44a971b53a642178e82efddce630b7115cee915577a4956398b85be69ae905ec"
	canonical := "POST\n/\n\n" +
		"content-type:application/x-www-form-urlencoded; charset=utf-8\n" +
		"host:iam.amazonaws.com\n" +
		"x-amz-content-sha256:" + bodyHash + "\n" +
		"x-amz-date:20150830T123600Z\n\n" +
		"content-type;host;x-amz-content-sha256;x-amz-date\n" + bodyHash
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/iam/aws4_request\n" + hex.EncodeToString(sum[:])
	key := []byte("AWS4" + testSecretKey)
	for _, part := range []string{"20150830", "us-east-1", "iam", "aws4_request", stringToSign} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(part))
		key = mac.Sum(nil)
	}

	want := http.Header{
		"Content-Type":         {"application/x-www-form-urlencoded; charset=utf-8"},
		"Host":                 {"iam.amazonaws.com"},
		"X-Amz-Date":           {"20150830T123600Z"},
		"X-Amz-Content-Sha256": {bodyHash},
		"Authorization": {"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20150830/us-east-1/iam/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=" + hex.EncodeToString(key)},
	}
	if d := cmp.Diff(want, req.Header); d != "" {
		t.Errorf("signed headers (-want +got):\n%s", d)
	}

	// A session token is sent and signed; a later time changes the scope.
	withToken, _ := http.NewRequest(http.MethodPost, "https://iam.amazonaws.com/", nil)
	creds.SessionToken = "sess-tok"
	if err := SignRequest(withToken, nil, creds, "eu-west-1", "sts", fixed.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	const prefix = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20150831/eu-west-1/sts/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature="
	if auth := withToken.Header.Get("Authorization"); !strings.HasPrefix(auth, prefix) || len(auth) != len(prefix)+64 || withToken.Header.Get("X-Amz-Security-Token") != "sess-tok" {
		t.Errorf("with a session token: Authorization = %q, token header %q", auth, withToken.Header.Get("X-Amz-Security-Token"))
	}
	if got := withToken.Header.Get("X-Amz-Content-Sha256"); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty body hash = %s", got)
	}

	if err := SignRequest(&http.Request{Header: http.Header{}}, nil, creds, "us-east-1", "iam", fixed); err == nil || err.Error() != "secret/aws: request URL is nil" {
		t.Errorf("SignRequest without a URL: %v", err)
	}
}
