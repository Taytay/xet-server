// Package sigv4 implements AWS Signature Version 4 request signing using
// only the Go standard library, so xet-server's S3-compatible storage backend
// needs no third-party SDK dependency. Implements both header-based signing
// (for PUT/GET/HEAD requests we construct ourselves) and query-string
// presigning (for handing a client a URL that can fetch an object directly).
//
// Reference: https://docs.aws.amazon.com/general/latest/gr/signature-version-4.html
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Algorithm       = "AWS4-HMAC-SHA256"
	UnsignedPayload = "UNSIGNED-PAYLOAD"

	dateFormat     = "20060102"
	timeFormat     = "20060102T150405Z"
	terminationTag = "aws4_request"
)

// Signer holds the credentials and scope (region/service) used to sign
// requests against an S3-compatible endpoint.
type Signer struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string // "s3"
}

func New(accessKey, secretKey, region, service string) *Signer {
	return &Signer{AccessKey: accessKey, SecretKey: secretKey, Region: region, Service: service}
}

// SignRequest signs req in place by setting X-Amz-Date, X-Amz-Content-Sha256,
// and Authorization headers. req.Host (or req.URL.Host if unset) is used as
// the Host header value for signing purposes; payloadHash must be the hex
// SHA-256 digest of the request body, or UnsignedPayload if there is none.
func (s *Signer) SignRequest(req *http.Request, payloadHash string, t time.Time) {
	date := t.UTC().Format(timeFormat)
	dateStamp := t.UTC().Format(dateFormat)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	req.Header.Set("X-Amz-Date", date)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, date)
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		canonicalQueryString(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/%s", dateStamp, s.Region, s.Service, terminationTag)
	stringToSign := strings.Join([]string{
		Algorithm,
		date,
		credentialScope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(s.signingSignature(dateStamp, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, s.AccessKey, credentialScope, signedHeaders, signature,
	))
}

// PresignURL returns rawURL with SigV4 query-string authentication added,
// valid for expirySeconds from now. Used to hand a client a URL that can
// GET an object directly from the storage backend without going through
// our own server.
func (s *Signer) PresignURL(method, rawURL string, expirySeconds int, t time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	date := t.UTC().Format(timeFormat)
	dateStamp := t.UTC().Format(dateFormat)
	credentialScope := fmt.Sprintf("%s/%s/%s/%s", dateStamp, s.Region, s.Service, terminationTag)

	q := u.Query()
	q.Set("X-Amz-Algorithm", Algorithm)
	q.Set("X-Amz-Credential", s.AccessKey+"/"+credentialScope)
	q.Set("X-Amz-Date", date)
	q.Set("X-Amz-Expires", strconv.Itoa(expirySeconds))
	q.Set("X-Amz-SignedHeaders", "host")

	canonicalQuery := canonicalQueryString(q)
	canonicalHeaders := "host:" + u.Host + "\n"
	const signedHeaders = "host"

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(u.Path),
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		UnsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		Algorithm,
		date,
		credentialScope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(s.signingSignature(dateStamp, stringToSign))

	u.RawQuery = canonicalQuery + "&X-Amz-Signature=" + signature
	return u.String(), nil
}

// signingSignature derives the SigV4 signing key for dateStamp and HMACs
// stringToSign with it.
func (s *Signer) signingSignature(dateStamp, stringToSign string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.SecretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(s.Region))
	kService := hmacSHA256(kRegion, []byte(s.Service))
	kSigning := hmacSHA256(kService, []byte(terminationTag))
	return hmacSHA256(kSigning, []byte(stringToSign))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// HashPayload returns the hex SHA-256 digest of data, for use as the
// payloadHash argument to SignRequest.
func HashPayload(data []byte) string {
	return hashHex(data)
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalURI URI-encodes each path segment per SigV4 rules, preserving
// slashes as segment separators. Does not normalize "." or ".." segments,
// matching S3's (not the generic SigV4) canonicalization rules.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg, true)
	}
	return strings.Join(segments, "/")
}

// canonicalQueryString URI-encodes and sorts query parameters per SigV4
// rules: sorted by encoded key, then by encoded value.
func canonicalQueryString(q url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		ek := uriEncode(k, true)
		for _, v := range vs {
			pairs = append(pairs, kv{ek, uriEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes s, leaving RFC 3986 unreserved characters
// (A-Z a-z 0-9 - _ . ~) untouched. If encodeSlash is false, '/' is also
// left untouched (used when encoding an already-slash-delimited path as a
// whole rather than per segment).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedByte(c) || (!encodeSlash && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreservedByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}
