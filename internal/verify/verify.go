// Package verify validates incoming webhook signatures. Every supported
// service signs its webhooks differently; a Verifier hides those differences
// behind a single interface.
package verify

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Timestamp-based schemes reject signatures older than this, matching the
// defaults used by the services' own SDKs.
const TimestampTolerance = 5 * time.Minute

// Errors returned by Verifiers. They are wrapped with details for logging.
var (
	ErrMissingSignature = errors.New("missing signature header")
	ErrMalformed        = errors.New("malformed signature")
	ErrInvalid          = errors.New("signature mismatch")
	ErrStale            = errors.New("signature timestamp outside tolerance")
	ErrUnsupportedBody  = errors.New("body is not form-encoded")
)

// Config describes how to verify webhooks for a route. Type and Secret are
// required for every type; the remaining fields are type-specific.
type Config struct {
	Type     string `yaml:"type"`
	Secret   string `yaml:"secret"`
	Header   string `yaml:"header"`   // hmac-sha256: header carrying the digest
	Encoding string `yaml:"encoding"` // hmac-sha256: hex (default) or base64
	URL      string `yaml:"url"`      // twilio: public URL senders post to
}

// Verifier validates a single incoming webhook request.
type Verifier interface {
	Verify(r *http.Request, body []byte) error
}

// New compiles a Config into a Verifier, validating type-specific fields.
func New(cfg Config) (Verifier, error) {
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, errors.New("webhook verification requires a secret")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "github":
		return githubVerifier{secret: []byte(cfg.Secret)}, nil
	case "gitlab":
		return gitlabVerifier{secret: cfg.Secret}, nil
	case "stripe":
		return stripeVerifier{secret: []byte(cfg.Secret)}, nil
	case "shopify":
		return hmacBodyVerifier{secret: []byte(cfg.Secret), header: "X-Shopify-Hmac-Sha256", encoding: "base64"}, nil
	case "slack":
		return slackVerifier{secret: []byte(cfg.Secret)}, nil
	case "twilio":
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, errors.New("twilio verification requires the url senders post to")
		}
		return twilioVerifier{secret: []byte(cfg.Secret), url: strings.TrimSuffix(cfg.URL, "?")}, nil
	case "pagerduty":
		return prefixHexVerifier{secret: []byte(cfg.Secret), header: "X-PagerDuty-Signature", prefix: "v1="}, nil
	case "gitea", "forgejo":
		return multiHeaderVerifier{secret: []byte(cfg.Secret), headers: []string{"X-Gitea-Signature", "X-Forgejo-Signature"}}, nil
	case "gogs":
		return hmacBodyVerifier{secret: []byte(cfg.Secret), header: "X-Gogs-Signature", encoding: "hex"}, nil
	case "standard", "svix":
		secret, err := standardSecret(cfg.Secret)
		if err != nil {
			return nil, err
		}
		return standardVerifier{secret: secret}, nil
	case "hmac-sha256":
		if strings.TrimSpace(cfg.Header) == "" {
			return nil, errors.New("hmac-sha256 verification requires a header name")
		}
		encoding := strings.ToLower(strings.TrimSpace(cfg.Encoding))
		if encoding == "" {
			encoding = "hex"
		}
		if encoding != "hex" && encoding != "base64" {
			return nil, fmt.Errorf("unknown digest encoding %q, want hex or base64", cfg.Encoding)
		}
		return hmacBodyVerifier{secret: []byte(cfg.Secret), header: cfg.Header, encoding: encoding}, nil
	case "token":
		if strings.TrimSpace(cfg.Header) == "" {
			return nil, errors.New("token verification requires a header name")
		}
		return tokenVerifier{secret: cfg.Secret, header: cfg.Header}, nil
	default:
		return nil, fmt.Errorf("unknown webhook verification type %q", cfg.Type)
	}
}

// githubVerifier checks X-Hub-Signature-256 (HMAC-SHA256) and the legacy
// X-Hub-Signature (HMAC-SHA1), in "sha256=<hexdigest>" form. Every header
// present must be valid, so old and new signatures can coexist.
type githubVerifier struct {
	secret []byte
}

func (v githubVerifier) Verify(r *http.Request, body []byte) error {
	present := false
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		present = true
		if err := checkHMACHex(sig, "sha256=", sha256.New, v.secret, body); err != nil {
			return err
		}
	}
	if sig := r.Header.Get("X-Hub-Signature"); sig != "" {
		present = true
		if err := checkHMACHex(sig, "sha1=", sha1.New, v.secret, body); err != nil {
			return err
		}
	}
	if !present {
		return ErrMissingSignature
	}
	return nil
}

// gitlabVerifier accepts either the plain X-Gitlab-Token header (compared in
// constant time) or the X-Gitlab-Signature HMAC-SHA256 hex digest.
type gitlabVerifier struct {
	secret string
}

func (v gitlabVerifier) Verify(r *http.Request, body []byte) error {
	present := false
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		present = true
		if !hmac.Equal([]byte(token), []byte(v.secret)) {
			return fmt.Errorf("%w: token mismatch", ErrInvalid)
		}
	}
	if sig := r.Header.Get("X-Gitlab-Signature"); sig != "" {
		present = true
		if err := checkHMACHex(sig, "sha256=", sha256.New, []byte(v.secret), body); err != nil {
			return err
		}
	}
	if !present {
		return ErrMissingSignature
	}
	return nil
}

// stripeVerifier checks the Stripe-Signature header ("t=<unix>,v1=<hex>,...")
// where the HMAC-SHA256 input is "<timestamp>.<body>".
type stripeVerifier struct {
	secret []byte
}

func (v stripeVerifier) Verify(r *http.Request, body []byte) error {
	header := r.Header.Get("Stripe-Signature")
	if header == "" {
		return ErrMissingSignature
	}
	var (
		timestamp string
		sigs      []string
	)
	for part := range strings.SplitSeq(header, ",") {
		k, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return fmt.Errorf("%w: expected k=v pairs", ErrMalformed)
		}
		switch k {
		case "t":
			timestamp = value
		case "v1":
			sigs = append(sigs, value)
		}
	}
	if timestamp == "" || len(sigs) == 0 {
		return fmt.Errorf("%w: missing timestamp or v1 signature", ErrMalformed)
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp %q", ErrMalformed, timestamp)
	}
	if !withinTolerance(ts) {
		return ErrStale
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range sigs {
		if hmac.Equal([]byte(strings.ToLower(sig)), []byte(expected)) {
			return nil
		}
	}
	return ErrInvalid
}

// slackVerifier checks X-Slack-Signature ("v0=<hex>") over the string
// "v0:<timestamp>:<body>" against X-Slack-Request-Timestamp.
type slackVerifier struct {
	secret []byte
}

func (v slackVerifier) Verify(r *http.Request, body []byte) error {
	sig := r.Header.Get("X-Slack-Signature")
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	if sig == "" || ts == "" {
		return ErrMissingSignature
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp %q", ErrMalformed, ts)
	}
	if !withinTolerance(seconds) {
		return ErrStale
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte("v0:"))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(strings.TrimPrefix(sig, "v0="))), []byte(expected)) {
		return ErrInvalid
	}
	return nil
}

// twilioVerifier checks X-Twilio-Signature: base64(HMAC-SHA1(authToken,
// url + sorted key/value pairs of the form-encoded body)). The URL is the
// public address the sender was configured to post to, so it is a
// configuration field rather than something read from the request.
type twilioVerifier struct {
	secret []byte
	url    string
}

func (v twilioVerifier) Verify(r *http.Request, body []byte) error {
	sig := r.Header.Get("X-Twilio-Signature")
	if sig == "" {
		return ErrMissingSignature
	}
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "application/x-www-form-urlencoded") {
		return ErrUnsupportedBody
	}
	params, err := url.ParseQuery(string(body))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnsupportedBody, err)
	}
	pairs := make([]string, 0, len(params))
	for key, values := range params {
		for _, value := range values {
			pairs = append(pairs, key+value)
		}
	}
	slices.Sort(pairs)
	mac := hmac.New(sha1.New, v.secret)
	mac.Write([]byte(v.url))
	for _, pair := range pairs {
		mac.Write([]byte(pair))
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrMalformed, err)
	}
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalid
	}
	return nil
}

// standardVerifier implements the Standard Webhooks specification (used by
// Svix and Svix-based senders): webhook-id, webhook-timestamp and a
// webhook-signature header of space-separated "v1,<base64>" digests of
// "<id>.<timestamp>.<body>".
type standardVerifier struct {
	secret []byte
}

func (v standardVerifier) Verify(r *http.Request, body []byte) error {
	id := r.Header.Get("Webhook-Id")
	ts := r.Header.Get("Webhook-Timestamp")
	sigs := r.Header.Values("Webhook-Signature")
	if id == "" || ts == "" || len(sigs) == 0 {
		return ErrMissingSignature
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp %q", ErrMalformed, ts)
	}
	if !withinTolerance(seconds) {
		return ErrStale
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	for _, header := range sigs {
		for sig := range strings.FieldsSeq(header) {
			encoded, found := strings.CutPrefix(sig, "v1,")
			if !found {
				return fmt.Errorf("%w: unsupported scheme %q", ErrMalformed, sig)
			}
			if decoded, err := decodeBase64(encoded); err == nil && hmac.Equal(decoded, mac.Sum(nil)) {
				return nil
			}
		}
	}
	return ErrInvalid
}

// hmacBodyVerifier checks a single header holding an HMAC-SHA256 digest of the
// raw body, hex- or base64-encoded. This covers services that use the common
// scheme under their own header name (Gitea, Linear, WooCommerce, ...).
type hmacBodyVerifier struct {
	secret   []byte
	header   string
	encoding string
}

func (v hmacBodyVerifier) Verify(r *http.Request, body []byte) error {
	sig := r.Header.Get(v.header)
	if sig == "" {
		return ErrMissingSignature
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write(body)
	sig = strings.TrimSpace(sig)
	sig, _ = strings.CutPrefix(sig, "sha256=")
	var (
		got  []byte
		err  error
		want = mac.Sum(nil)
	)
	switch v.encoding {
	case "hex":
		got, err = hex.DecodeString(strings.ToLower(sig))
	case "base64":
		got, err = decodeBase64(sig)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", ErrMalformed, err)
	}
	if !hmac.Equal(got, want) {
		return ErrInvalid
	}
	return nil
}

// multiHeaderVerifier validates hex HMAC-SHA256 digests of the raw body under
// any of several headers; every header present must be valid and at least one
// is required. Forgejo is a Gitea fork and sends the Gitea header, with the
// Forgejo-named twin alongside it on newer versions.
type multiHeaderVerifier struct {
	secret  []byte
	headers []string
}

func (v multiHeaderVerifier) Verify(r *http.Request, body []byte) error {
	mac := hmac.New(sha256.New, v.secret)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	present := false
	for _, header := range v.headers {
		sig := r.Header.Get(header)
		if sig == "" {
			continue
		}
		present = true
		if !hmac.Equal([]byte(strings.ToLower(strings.TrimSpace(sig))), []byte(expected)) {
			return ErrInvalid
		}
	}
	if !present {
		return ErrMissingSignature
	}
	return nil
}

// tokenVerifier compares a header value against the shared secret in constant
// time. Neither Docker Hub nor Docker Distribution signs its webhook
// deliveries; Distribution can be configured to attach a static header, which
// this checks.
type tokenVerifier struct {
	secret string
	header string
}

func (v tokenVerifier) Verify(r *http.Request, body []byte) error {
	got := r.Header.Get(v.header)
	if got == "" {
		return fmt.Errorf("%w: %s", ErrMissingSignature, v.header)
	}
	if !hmac.Equal([]byte(got), []byte(v.secret)) {
		return ErrInvalid
	}
	return nil
}

// prefixHexVerifier checks a header holding "<prefix><hex hmac-sha256>" of the
// raw body, which is PagerDuty's scheme.
type prefixHexVerifier struct {
	secret []byte
	header string
	prefix string
}

func (v prefixHexVerifier) Verify(r *http.Request, body []byte) error {
	return checkHMACHex(r.Header.Get(v.header), v.prefix, sha256.New, v.secret, body)
}

func checkHMACHex(sig, prefix string, newHash func() hash.Hash, secret, body []byte) error {
	if sig == "" {
		return ErrMissingSignature
	}
	mac := hmac.New(newHash, secret)
	mac.Write(body)
	sig = strings.TrimSpace(sig)
	sig, _ = strings.CutPrefix(sig, prefix)
	if !hmac.Equal([]byte(strings.ToLower(sig)), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return ErrInvalid
	}
	return nil
}

// standardSecret strips the whsec_ prefix Standard Webhooks keys carry and
// decodes the base64 payload; keys without the prefix are used verbatim.
func standardSecret(secret string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		return []byte(secret), nil
	}
	decoded, err := decodeBase64(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode whsec_ secret: %w", err)
	}
	return decoded, nil
}

func decodeBase64(s string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func withinTolerance(unixSeconds int64) bool {
	delta := time.Since(time.Unix(unixSeconds, 0))
	if delta < 0 {
		delta = -delta
	}
	return delta <= TimestampTolerance
}
