# Hookmux

Hookmux is a webhook multiplexer. It receives webhooks from any service on a
single endpoint, decides where they should go by evaluating
[jq](https://jqlang.github.io/jq/) expressions against the payload, and then
forwards them to other HTTP or HTTPS hook receivers, executes local commands,
or silently discards them.

Routing rules live in a YAML file; everything else is configured with
command-line flags (which can also be set through environment variables).

## Usage

```
hookmux [--listen :8080] [--config config.yml] [--timeout 30s] [--max-body 10485760] [log flags]
```

| Flag               | Environment variable | Default   | Description                                            |
|--------------------|----------------------|-----------|--------------------------------------------------------|
| `--listen`         | `LISTEN`             | `:8080`   | Address to listen for incoming webhooks on.            |
| `--config`         | `CONFIG`             | `config.yml` | Path to the YAML routing configuration.             |
| `--timeout`        | `TIMEOUT`            | `30s`     | Maximum time allowed per forward or exec action.       |
| `--max-body`       | `MAX_BODY`           | `10485760`| Maximum accepted webhook body size, in bytes.          |
| `--log.level`      |                      | `info`    | Log level: `debug`, `info`, `warn` or `error`.         |
| `--log.format`     |                      | `text`    | Log format: `text` or `json`.                          |

Environment variable names are the flag name upper-cased with dashes replaced
by underscores. Command-line flags take precedence over environment variables.

`GET /` serves a small HTML landing page describing the endpoints. `GET
/healthz` returns `200 OK` for liveness checks.

## The webhook endpoint

All webhooks arrive at the root endpoint — any path works, since routing is
based purely on the payload:

```
POST /
```

`PUT` and `PATCH` are accepted as well, and sub-paths like `/github` or
`/alertmanager` behave identically, which is handy for services that insist on
posting to their own paths. The request body should be JSON, since
that is what jq expressions evaluate against, but any payload is accepted —
non-JSON payloads simply never match a route that has a `match` expression,
while catch-all routes (no `match`) still receive the raw body. The response is
always a quick `202 Accepted`; matching and delivery happen in the background
so slow destinations never block the sender.

## Routing configuration

See [`config.example.yml`](config.example.yml) for a complete example.

```yaml
routes:
  - name: github-pushes
    match: '.repository != null and .event == "push"'
    forward:
      - url: https://ci.example.com/hooks/github
        method: POST
        headers:
          X-Webhook-Token: super-secret-token
        body: '{event: .event, repo: .repository.name}'
    exec:
      - command: ["/usr/local/bin/notify", "--channel", "ops"]
```

Each route supports:

| Field    | Required | Description                                                                                             |
|----------|----------|---------------------------------------------------------------------------------------------------------|
| `name`   | no       | Human-readable name used in logs and the `HOOKMUX_ROUTE` environment variable.                           |
| `match`  | no       | A jq expression evaluated against the parsed JSON payload. The route matches if it returns anything other than `false` or `null`. Omit it to match every webhook. |
| `verify` | no       | Signature verification for incoming webhooks. The route only matches requests whose signature validates (see below). |
| `forward`| no       | Zero or more HTTP(S) deliveries (see below).                                                             |
| `exec`   | no       | Zero or more commands to run (see below).                                                                |
| `noop`   | no       | Set to `true` to make the route do nothing: matched webhooks are accepted and discarded. Useful for explicitly ignoring events. |

A route must have at least one `forward` or `exec` action, or `noop: true`. **Every** matching
route fires, and all of a route's actions run concurrently — so you can fan a
single webhook out to many destinations, and a payload that matches several
routes is delivered to all of them.

### Verifying webhooks

A route can require proof that the webhook came from the real sender. Give it a
`verify` block and the route only matches requests whose signature validates
against the configured secret; failed or missing signatures are logged and the
route is skipped (the sender still receives a `202`). A route can have both a
`match` expression and a `verify` block — both must pass.

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

Every service signs its webhooks differently, so `type` selects the scheme and
`secret` is always required. A few types need one extra field.

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

The schemes that carry a timestamp (`stripe`, `slack`, `standard`) reject
signatures older or newer than five minutes from the local clock, so the host
clock should be roughly correct.

#### Docker Hub and Docker Distribution

Neither Docker Hub nor Docker Distribution signs its webhook deliveries, so
there is no signature to verify:

- **Docker Hub** provides no signature or shared secret at all. Do not rely on
  a hookmux route alone to gatekeep Docker Hub webhooks on a public endpoint —
  terminate them behind something that authenticates (reverse proxy, VPN) and
  send them to a route without a `verify` block. `match` expressions such as
  `'.push_data != null'` decide which routes fire, but they are not
  authentication.
- **Docker Distribution** (registry v2) can attach static headers to its
  notifications. Configure a token in the registry config and verify it with
  the `token` type:

  ```yaml
  # registry config
  notifications:
    endpoints:
      - name: hookmux
        url: https://hookmux.example.com/docker
        headers:
          X-Hook-Token:
            - registry-webhook-token
  ```

  ```yaml
  # hookmux route
  verify:
    type: token
    secret: registry-webhook-token
    header: X-Hook-Token
  ```

### Forward actions

| Field     | Required | Description                                                                                     |
|-----------|----------|-------------------------------------------------------------------------------------------------|
| `url`     | yes      | Destination URL. Both `http://` and `https://` are supported.                                    |
| `method`  | no       | HTTP method, defaults to `POST`.                                                                 |
| `headers` | no       | Extra headers to set, e.g. authentication tokens. They override the defaults described below.    |
| `body`    | no       | A jq expression that transforms the payload; the result is sent as JSON. Without it the raw request body is forwarded unchanged. |

Forwarded requests are `POST`ed with the original `Content-Type` (unless
overridden), a `User-Agent` of `hookmux`, the caller's address in
`X-Forwarded-For`, and an `X-Hookmux-Route` header naming the route.

### Exec actions

| Field     | Required | Description                                     |
|-----------|----------|-------------------------------------------------|
| `command` | yes      | The command and its arguments, as a YAML array. |

The raw webhook body is piped to the command's stdin, `HOOKMUX_ROUTE` is set to
the route name in its environment, and a non-zero exit code is logged as an
error.

## jq tips

- The payload is the jq input: `.event == "push"`, `.alerts | length > 0`,
  `.head_commit.id` all work as match expressions.
- `body` transforms are ordinary jq expressions: `{status: .status}` builds a
  new object; `[.alerts[].labels.alertname]` builds an array.
- Number precision is preserved (`json.Number`), so large IDs survive
  transforms intact.

## Building

Requires Go 1.25+.

```
go build -o hookmux .
go test ./...
```

Run it against the example config:

```
hookmux --listen :8080 --config config.example.yml
curl -X POST http://localhost:8080/ -H 'Content-Type: application/json' \
     -d '{"event":"push","repository":{"name":"hookmux"}}'
```
