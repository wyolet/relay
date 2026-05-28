package aws

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
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
	if err := signRequest(req, body, creds, "us-east-1", "iam", fixed); err != nil {
		t.Fatal(err)
	}

	auth := req.Header.Get("Authorization")

	hdr := req.Header.Clone()
	hdr.Del("Authorization")
	signed := signedHeaderNames(hdr)
	canonical := buildCanonicalRequest(http.MethodPost, "/", "", hdr, signed, sha256Hex(body))
	canonicalHash := sha256Hex([]byte(canonical))
	scope := "20150830/us-east-1/iam/aws4_request"
	stringToSign := algorithm + "\n20150830T123600Z\n" + scope + "\n" + canonicalHash
	key := deriveSigningKey(testSecretKey, "20150830", "us-east-1", "iam")
	wantSig := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	if !strings.Contains(auth, "Signature="+wantSig) {
		t.Fatalf("signature: got auth %q want sig %s", auth, wantSig)
	}
	if !strings.Contains(auth, "Credential=AKIAIOSFODNN7EXAMPLE/20150830/us-east-1/iam/aws4_request") {
		t.Fatalf("credential scope missing in %q", auth)
	}
}
