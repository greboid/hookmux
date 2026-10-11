# Verifying webhooks

A route can require proof that the webhook came from the real sender. Give it a `verify` block and the route only matches requests whose signature validates against the configured secret; failed or missing signatures are logged and the route is skipped (the sender still receives a `202`). A route can have both a `match` expression and a `verify` block — both must pass.

```yaml
routes:
  - name: github-pushes
    match: '.repository != null'
    verify:
      type: github
      secret: github-webhook-secret
    forward:
      - url: https://ci.example.com/hooks/github
```

Every service signs its webhooks differently, so `type` selects the scheme and `secret` is always required. A few types need one extra field.

| Type        | Signature scheme                                                                                                       | Extra fields |
|-------------|------------------------------------------------------------------------------------------------------------------------|--------------|
| `github`    | `X-Hub-Signature-256: sha256=<hex>` (HMAC-SHA256); the legacy `X-Hub-Signature` SHA-1 header is also checked when present |              |
| `gitlab`    | `X-Gitlab-Token: <token>` and/or `X-Gitlab-Signature: sha256=<hex>` — whichever of the two is present must be valid       |              |
| `stripe`    | `Stripe-Signature: t=<unix>,v1=<hex>` — HMAC-SHA256 of `<timestamp>.<body>`                                              |              |
| `shopify`   | `X-Shopify-Hmac-Sha256: <base64>` — HMAC-SHA256 of the raw body                                                          |              |
| `slack`     | `X-Slack-Signature: v0=<hex>` — HMAC-SHA256 of `v0:<timestamp>:<body>`                                                   |              |
| `twilio`    | `X-Twilio-Signature: <base64>` — HMAC-SHA1 of the request URL plus the sorted form parameters                            | `url`: the public URL senders post to, protocol through path, no query string |
| `pagerduty` | `X-PagerDuty-Signature: v1=<hex>` — HMAC-SHA256 of the raw body                                                          |              |
| `gitea`     | `X-Gitea-Signature: <hex>` — HMAC-SHA256 of the raw body; Forgejo's `X-Forgejo-Signature` twin is also checked when present |              |
| `gogs`      | `X-Gogs-Signature: <hex>` — HMAC-SHA256 of the raw body                                                                   |              |
| `standard`  | [Standard Webhooks](https://www.standardwebhooks.com) (Svix and Svix-based senders): `webhook-id`, `webhook-timestamp` and `webhook-signature: v1,<base64>`; `whsec_`-prefixed secrets are supported | |
| `hmac-sha256` | HMAC-SHA256 of the raw body in a header of your choice — covers Linear, WooCommerce and other services that use the common scheme under their own header name | `header`: header carrying the digest; `encoding`: `hex` (default) or `base64` |
| `token` | A static header value compared in constant time — for services that authenticate with a shared token instead of a signature (Docker Distribution, many CI systems) | `header`: header carrying the token |

The schemes that carry a timestamp (`stripe`, `slack`, `standard`) reject signatures older or newer than five minutes from the local clock, so the host clock should be roughly correct.
