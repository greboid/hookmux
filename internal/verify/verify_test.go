package verify

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSecret = "it's a secret to everybody"
	testBody   = `{"event":"push","repository":{"name":"hookmux"}}`
	testURL    = "https://hooks.example.com/twilio"
)

func mac(h func() hash.Hash, secret string, parts ...string) []byte {
	m := hmac.New(h, []byte(secret))
	for _, part := range parts {
		m.Write([]byte(part))
	}
	return m.Sum(nil)
}

func hexMAC(h func() hash.Hash, secret string, parts ...string) string {
	return hex.EncodeToString(mac(h, secret, parts...))
}

func b64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func request(t *testing.T, headers map[string]string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// whsecSecret returns the test secret in the prefixed form Standard Webhooks
// senders use.
func whsecSecret() string {
	return "whsec_" + b64([]byte(testSecret))
}

func standardHeaders(id, ts, sig string) map[string]string {
	return map[string]string{"Webhook-Id": id, "Webhook-Timestamp": ts, "Webhook-Signature": sig}
}

func nowUnix() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func oldUnix() string {
	return strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
}

func TestValidSignaturesAccepted(t *testing.T) {
	now := nowUnix()
	const formBody = "a=1&b=2"
	tests := []struct {
		name    string
		cfg     Config
		headers map[string]string
		body    string
	}{
		{
			name:    "github sha256",
			cfg:     Config{Type: "github", Secret: testSecret},
			headers: map[string]string{"X-Hub-Signature-256": "sha256=" + hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "github legacy sha1",
			cfg:     Config{Type: "github", Secret: testSecret},
			headers: map[string]string{"X-Hub-Signature": "sha1=" + hexMAC(sha1.New, testSecret, testBody)},
		},
		{
			name: "github both signatures",
			cfg:  Config{Type: "github", Secret: testSecret},
			headers: map[string]string{
				"X-Hub-Signature-256": "sha256=" + hexMAC(sha256.New, testSecret, testBody),
				"X-Hub-Signature":     "sha1=" + hexMAC(sha1.New, testSecret, testBody),
			},
		},
		{
			name:    "gitlab token",
			cfg:     Config{Type: "gitlab", Secret: testSecret},
			headers: map[string]string{"X-Gitlab-Token": testSecret},
		},
		{
			name:    "gitlab signature",
			cfg:     Config{Type: "gitlab", Secret: testSecret},
			headers: map[string]string{"X-Gitlab-Signature": "sha256=" + hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name: "gitlab token and signature",
			cfg:  Config{Type: "gitlab", Secret: testSecret},
			headers: map[string]string{
				"X-Gitlab-Token":     testSecret,
				"X-Gitlab-Signature": "sha256=" + hexMAC(sha256.New, testSecret, testBody),
			},
		},
		{
			name:    "stripe",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=" + now + ",v1=" + hexMAC(sha256.New, testSecret, now+".", testBody)},
		},
		{
			name:    "stripe extra v1 signatures",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=" + now + ",v1=deadbeef,v1=" + hexMAC(sha256.New, testSecret, now+".", testBody)},
		},
		{
			name:    "shopify",
			cfg:     Config{Type: "shopify", Secret: testSecret},
			headers: map[string]string{"X-Shopify-Hmac-Sha256": b64(mac(sha256.New, testSecret, testBody))},
		},
		{
			name:    "slack",
			cfg:     Config{Type: "slack", Secret: testSecret},
			headers: map[string]string{"X-Slack-Request-Timestamp": now, "X-Slack-Signature": "v0=" + hexMAC(sha256.New, testSecret, "v0:"+now+":"+testBody)},
		},
		{
			name:    "twilio",
			cfg:     Config{Type: "twilio", Secret: testSecret, URL: testURL},
			headers: map[string]string{"X-Twilio-Signature": b64(mac(sha1.New, testSecret, testURL, "a1", "b2")), "Content-Type": "application/x-www-form-urlencoded"},
			body:    formBody,
		},
		{
			name:    "pagerduty",
			cfg:     Config{Type: "pagerduty", Secret: testSecret},
			headers: map[string]string{"X-PagerDuty-Signature": "v1=" + hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "gitea",
			cfg:     Config{Type: "gitea", Secret: testSecret},
			headers: map[string]string{"X-Gitea-Signature": hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "forgejo",
			cfg:     Config{Type: "forgejo", Secret: testSecret},
			headers: map[string]string{"X-Forgejo-Signature": hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name: "forgejo with both signature headers",
			cfg:  Config{Type: "forgejo", Secret: testSecret},
			headers: map[string]string{
				"X-Gitea-Signature":   hexMAC(sha256.New, testSecret, testBody),
				"X-Forgejo-Signature": hexMAC(sha256.New, testSecret, testBody),
			},
		},
		{
			name:    "gogs",
			cfg:     Config{Type: "gogs", Secret: testSecret},
			headers: map[string]string{"X-Gogs-Signature": hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "standard webhooks with whsec_ secret",
			cfg:     Config{Type: "standard", Secret: whsecSecret()},
			headers: standardHeaders("msg_1", now, "v1,"+b64(mac(sha256.New, testSecret, "msg_1.", now, ".", testBody))),
		},
		{
			name:    "standard webhooks with raw secret",
			cfg:     Config{Type: "standard", Secret: testSecret},
			headers: standardHeaders("msg_1", now, "v1,"+b64(mac(sha256.New, testSecret, "msg_1.", now, ".", testBody))),
		},
		{
			name: "standard webhooks first signature invalid",
			cfg:  Config{Type: "standard", Secret: whsecSecret()},
			headers: standardHeaders("msg_1", now,
				"v1,"+b64([]byte("nonsense"))+" v1,"+b64(mac(sha256.New, testSecret, "msg_1.", now, ".", testBody))),
		},
		{
			name:    "generic hex",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature"},
			headers: map[string]string{"X-Custom-Signature": hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "generic hex with sha256= prefix",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature"},
			headers: map[string]string{"X-Custom-Signature": "sha256=" + hexMAC(sha256.New, testSecret, testBody)},
		},
		{
			name:    "generic base64",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature", Encoding: "base64"},
			headers: map[string]string{"X-Custom-Signature": b64(mac(sha256.New, testSecret, testBody))},
		},
		{
			name:    "token",
			cfg:     Config{Type: "token", Secret: testSecret, Header: "X-Hook-Token"},
			headers: map[string]string{"X-Hook-Token": testSecret},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			body := tt.body
			if body == "" {
				body = testBody
			}
			if err := v.Verify(request(t, tt.headers, body), []byte(body)); err != nil {
				t.Errorf("Verify() error = %v, want nil", err)
			}
		})
	}
}

func TestSignaturesRejected(t *testing.T) {
	now := nowUnix()
	stale := oldUnix()
	standardID := "msg_1"
	validStandardSig := "v1," + b64(mac(sha256.New, testSecret, standardID+".", now, ".", testBody))
	tests := []struct {
		name    string
		cfg     Config
		headers map[string]string
		body    string
		want    error
	}{
		{
			name:    "github wrong secret",
			cfg:     Config{Type: "github", Secret: testSecret},
			headers: map[string]string{"X-Hub-Signature-256": "sha256=" + hexMAC(sha256.New, "other-secret", testBody)},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "github tampered body",
			cfg:     Config{Type: "github", Secret: testSecret},
			headers: map[string]string{"X-Hub-Signature-256": "sha256=" + hexMAC(sha256.New, testSecret, testBody)},
			body:    `{"event":"push","repository":{"name":"evil"}}`,
			want:    ErrInvalid,
		},
		{
			name:    "github missing headers",
			cfg:     Config{Type: "github", Secret: testSecret},
			headers: map[string]string{},
			body:    testBody,
			want:    ErrMissingSignature,
		},
		{
			name: "github bad sha256 alongside good sha1",
			cfg:  Config{Type: "github", Secret: testSecret},
			headers: map[string]string{
				"X-Hub-Signature-256": "sha256=" + hexMAC(sha256.New, "other-secret", testBody),
				"X-Hub-Signature":     "sha1=" + hexMAC(sha1.New, testSecret, testBody),
			},
			body: testBody,
			want: ErrInvalid,
		},
		{
			name:    "gitlab wrong token with good signature",
			cfg:     Config{Type: "gitlab", Secret: testSecret},
			headers: map[string]string{"X-Gitlab-Token": "nope", "X-Gitlab-Signature": "sha256=" + hexMAC(sha256.New, testSecret, testBody)},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "gitlab no headers",
			cfg:     Config{Type: "gitlab", Secret: testSecret},
			headers: map[string]string{},
			body:    testBody,
			want:    ErrMissingSignature,
		},
		{
			name:    "stripe stale timestamp",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=" + stale + ",v1=" + hexMAC(sha256.New, testSecret, stale+".", testBody)},
			body:    testBody,
			want:    ErrStale,
		},
		{
			name:    "stripe missing timestamp",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "v1=" + hexMAC(sha256.New, testSecret, now+".", testBody)},
			body:    testBody,
			want:    ErrMalformed,
		},
		{
			name:    "stripe missing signature",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=" + now},
			body:    testBody,
			want:    ErrMalformed,
		},
		{
			name:    "stripe bad timestamp",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=not-a-number,v1=deadbeef"},
			body:    testBody,
			want:    ErrMalformed,
		},
		{
			name:    "stripe wrong secret",
			cfg:     Config{Type: "stripe", Secret: testSecret},
			headers: map[string]string{"Stripe-Signature": "t=" + now + ",v1=" + hexMAC(sha256.New, "other-secret", now+".", testBody)},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "slack stale timestamp",
			cfg:     Config{Type: "slack", Secret: testSecret},
			headers: map[string]string{"X-Slack-Request-Timestamp": stale, "X-Slack-Signature": "v0=" + hexMAC(sha256.New, testSecret, "v0:"+stale+":"+testBody)},
			body:    testBody,
			want:    ErrStale,
		},
		{
			name:    "slack missing timestamp header",
			cfg:     Config{Type: "slack", Secret: testSecret},
			headers: map[string]string{"X-Slack-Signature": "v0=" + hexMAC(sha256.New, testSecret, "v0:"+now+":"+testBody)},
			body:    testBody,
			want:    ErrMissingSignature,
		},
		{
			name:    "twilio wrong url",
			cfg:     Config{Type: "twilio", Secret: testSecret, URL: "https://hooks.example.com/other"},
			headers: map[string]string{"X-Twilio-Signature": b64(mac(sha1.New, testSecret, testURL, "a1", "b2"))},
			body:    "a=1&b=2",
			want:    ErrInvalid,
		},
		{
			name:    "twilio json body",
			cfg:     Config{Type: "twilio", Secret: testSecret, URL: testURL},
			headers: map[string]string{"X-Twilio-Signature": b64(mac(sha1.New, testSecret, testURL)), "Content-Type": "application/json"},
			body:    testBody,
			want:    ErrUnsupportedBody,
		},
		{
			name:    "pagerduty tampered body",
			cfg:     Config{Type: "pagerduty", Secret: testSecret},
			headers: map[string]string{"X-PagerDuty-Signature": "v1=" + hexMAC(sha256.New, testSecret, testBody)},
			body:    "tampered",
			want:    ErrInvalid,
		},
		{
			name:    "gitea wrong secret",
			cfg:     Config{Type: "gitea", Secret: testSecret},
			headers: map[string]string{"X-Gitea-Signature": hexMAC(sha256.New, "other-secret", testBody)},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "gitea missing signature",
			cfg:     Config{Type: "gitea", Secret: testSecret},
			headers: map[string]string{},
			body:    testBody,
			want:    ErrMissingSignature,
		},
		{
			name: "forgejo bad gitea header alongside good forgejo header",
			cfg:  Config{Type: "forgejo", Secret: testSecret},
			headers: map[string]string{
				"X-Gitea-Signature":   hexMAC(sha256.New, "other-secret", testBody),
				"X-Forgejo-Signature": hexMAC(sha256.New, testSecret, testBody),
			},
			body: testBody,
			want: ErrInvalid,
		},
		{
			name:    "token wrong secret",
			cfg:     Config{Type: "token", Secret: testSecret, Header: "X-Hook-Token"},
			headers: map[string]string{"X-Hook-Token": "not-the-secret"},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "token missing header",
			cfg:     Config{Type: "token", Secret: testSecret, Header: "X-Hook-Token"},
			headers: map[string]string{},
			body:    testBody,
			want:    ErrMissingSignature,
		},
		{
			name:    "standard stale timestamp",
			cfg:     Config{Type: "standard", Secret: whsecSecret()},
			headers: standardHeaders(standardID, stale, "v1,"+b64(mac(sha256.New, testSecret, standardID+".", stale, ".", testBody))),
			body:    testBody,
			want:    ErrStale,
		},
		{
			name:    "standard wrong signature",
			cfg:     Config{Type: "standard", Secret: whsecSecret()},
			headers: standardHeaders(standardID, now, "v1,"+b64(mac(sha256.New, "other-secret", standardID+".", now, ".", testBody))),
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "standard unsupported scheme",
			cfg:     Config{Type: "standard", Secret: whsecSecret()},
			headers: standardHeaders(standardID, now, "v0,"+validStandardSig),
			body:    testBody,
			want:    ErrMalformed,
		},
		{
			name:    "generic wrong digest",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature"},
			headers: map[string]string{"X-Custom-Signature": hexMAC(sha1.New, testSecret, testBody)},
			body:    testBody,
			want:    ErrInvalid,
		},
		{
			name:    "generic garbage digest",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature"},
			headers: map[string]string{"X-Custom-Signature": "not-hex!"},
			body:    testBody,
			want:    ErrMalformed,
		},
		{
			name:    "generic missing header",
			cfg:     Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Custom-Signature"},
			headers: map[string]string{"X-Other-Signature": "aa"},
			body:    testBody,
			want:    ErrMissingSignature,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = v.Verify(request(t, tt.headers, tt.body), []byte(tt.body))
			if !errors.Is(err, tt.want) {
				t.Errorf("Verify() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestNewConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing secret", Config{Type: "github"}},
		{"blank secret", Config{Type: "github", Secret: "   "}},
		{"unknown type", Config{Type: "carrier-pigeon", Secret: testSecret}},
		{"missing type", Config{Secret: testSecret}},
		{"twilio without url", Config{Type: "twilio", Secret: testSecret}},
		{"hmac-sha256 without header", Config{Type: "hmac-sha256", Secret: testSecret}},
		{"hmac-sha256 bad encoding", Config{Type: "hmac-sha256", Secret: testSecret, Header: "X-Sig", Encoding: "rot13"}},
		{"standard secret not base64", Config{Type: "standard", Secret: "whsec_!!not-base64!!"}},
		{"token without header", Config{Type: "token", Secret: testSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Errorf("New() succeeded, want error")
			}
		})
	}
}

func TestNewIsCaseInsensitive(t *testing.T) {
	for _, typ := range []string{"GitHub", "GITLAB", " Stripe ", "svix", "Standard", "Gitea", " Forgejo ", "GOGS", " Token "} {
		if _, err := New(Config{Type: typ, Secret: testSecret, Header: "X-Sig", URL: testURL}); err != nil {
			t.Errorf("New(%q) error = %v", typ, err)
		}
	}
}

func TestStandardTimestampTypes(t *testing.T) {
	// A timestamp slightly in the future (clock skew) must still verify.
	future := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	v, err := New(Config{Type: "standard", Secret: whsecSecret()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	headers := standardHeaders("msg_1", future, "v1,"+b64(mac(sha256.New, testSecret, "msg_1.", future, ".", testBody)))
	if err := v.Verify(request(t, headers, testBody), []byte(testBody)); err != nil {
		t.Errorf("Verify() with future timestamp error = %v, want nil", err)
	}
}
