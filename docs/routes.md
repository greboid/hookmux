# Routing configuration

Routes live in the YAML file given by `--config`. See
[`config.example.yml`](../config.example.yml) for a complete example.

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

| Field      | Required | Description                                                                                             |
|------------|----------|---------------------------------------------------------------------------------------------------------|
| `name`     | no       | Human-readable name used in logs and the `HOOKMUX_ROUTE` environment variable.                           |
| `match`    | no       | A jq expression evaluated against the parsed JSON payload. The route matches if it returns anything other than `false` or `null`. Omit it to match every webhook. See [matching](matching.md). |
| `matchpath`| no       | One or more URL path prefixes the request path must match (see [matching](matching.md)). Accepts a single string or a list. Omit it to match any path. |
| `verify`   | no       | Signature verification for incoming webhooks. The route only matches requests whose signature validates (see [verification](verify.md)). |
| `forward`  | no       | Zero or more HTTP(S) deliveries (see below).                                                             |
| `exec`     | no       | Zero or more commands to run (see below).                                                                |
| `noop`     | no       | Set to `true` to make the route do nothing: matched webhooks are accepted and discarded. Useful for explicitly ignoring events. |

A route must have at least one `forward` or `exec` action, or `noop: true`. **Every** matching
route fires, and all of a route's actions run concurrently — so you can fan a
single webhook out to many destinations, and a payload that matches several
routes is delivered to all of them.

## Forward actions

| Field     | Required | Description                                                                                     |
|-----------|----------|-------------------------------------------------------------------------------------------------|
| `url`     | yes      | Destination URL. Both `http://` and `https://` are supported.                                    |
| `method`  | no       | HTTP method, defaults to `POST`.                                                                 |
| `headers` | no       | Extra headers to set, e.g. authentication tokens. They override the defaults described below.    |
| `body`    | no       | A jq expression that transforms the payload; the result is sent as JSON. Without it the raw request body is forwarded unchanged. |

Forwarded requests are `POST`ed with the original `Content-Type` (unless
overridden), a `User-Agent` of `hookmux`, the caller's address in
`X-Forwarded-For`, and an `X-Hookmux-Route` header naming the route.

## Exec actions

| Field     | Required | Description                                     |
|-----------|----------|-------------------------------------------------|
| `command` | yes      | The command and its arguments, as a YAML array. |

The raw webhook body is piped to the command's stdin, `HOOKMUX_ROUTE` is set to
the route name in its environment, and a non-zero exit code is logged as an
error.
