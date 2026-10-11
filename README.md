# Hookmux

Hookmux is a webhook multiplexer. It receives webhooks from any service on a
single endpoint, decides where they should go by evaluating
[jq](https://jqlang.github.io/jq/) expressions against the payload (and
optionally matching on the request path), and then
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
based on the payload:

```
POST /
```

`PUT` and `PATCH` are accepted as well, and sub-paths like `/github` or
`/alertmanager` behave identically, which is handy for services that insist on
posting to their own paths. Routes can key off the path with a
[`matchpath`](docs/matching.md) prefix. The request body should be JSON, since
that is what jq expressions evaluate against, but any payload is accepted. The
response is always a quick `202 Accepted`; matching and delivery happen in the
background so slow destinations never block the sender.

## Routing

See [`config.example.yml`](config.example.yml) for a complete example.

```yaml
routes:
  - name: github-pushes
    match: '.repository != null and .event == "push"'
    forward:
      - url: https://ci.example.com/hooks/github
    exec:
      - command: ["/usr/local/bin/notify", "--channel", "ops"]
```

Every matching route fires, and all of a route's actions run concurrently, so
a single webhook can fan out to many destinations. Routes can also
[verify sender signatures](docs/verify.md) and [match on the request
path](docs/matching.md).

- [Routing configuration](docs/routes.md) — route fields, forward and exec actions
- [Matching](docs/matching.md) — jq payload expressions and path prefixes
- [Verifying webhooks](docs/verify.md) — signature schemes for GitHub, GitLab, Stripe and more

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
