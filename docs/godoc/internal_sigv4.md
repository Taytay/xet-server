# `xet-server/internal/sigv4`

```
package sigv4 // import "xet-server/internal/sigv4"

Package sigv4 implements AWS Signature Version 4 request signing using only
the Go standard library, so xet-server's S3-compatible storage backend needs
no third-party SDK dependency. Implements both header-based signing (for
PUT/GET/HEAD requests we construct ourselves) and query-string presigning (for
handing a client a URL that can fetch an object directly).

Reference:
https://docs.aws.amazon.com/general/latest/gr/signature-version-4.html

CONSTANTS

const (
	Algorithm       = "AWS4-HMAC-SHA256"
	UnsignedPayload = "UNSIGNED-PAYLOAD"
)

FUNCTIONS

func HashPayload(data []byte) string
    HashPayload returns the hex SHA-256 digest of data, for use as the
    payloadHash argument to SignRequest.


TYPES

type Signer struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string // "s3"
}
    Signer holds the credentials and scope (region/service) used to sign
    requests against an S3-compatible endpoint.

func New(accessKey, secretKey, region, service string) *Signer

func (s *Signer) PresignURL(method, rawURL string, expirySeconds int, t time.Time) (string, error)
    PresignURL returns rawURL with SigV4 query-string authentication added,
    valid for expirySeconds from now. Used to hand a client a URL that can GET
    an object directly from the storage backend without going through our own
    server.

func (s *Signer) SignRequest(req *http.Request, payloadHash string, t time.Time)
    SignRequest signs req in place by setting X-Amz-Date, X-Amz-Content-Sha256,
    and Authorization headers. req.Host (or req.URL.Host if unset) is used as
    the Host header value for signing purposes; payloadHash must be the hex
    SHA-256 digest of the request body, or UnsignedPayload if there is none.
```
