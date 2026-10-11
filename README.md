# Hookmux

Hookmux is a webhook multiplexer.  It receives webhooks from any service on a single endpoint, decides where they should go by evaluating matches, then forwards the result on to another webhook receiver or a local executable.

Routes can be configured based on matching path and/or JSON payload - if matched these can be forwarded to other services (as is or edited) or passed to a local executable.  Every matching route fires its actions concurrently.

The webhook endpoint accepts `POST`, `PUT` and `PATCH` requests; anything else gets a `405`.

Routing rules live in a YAML file; everything else is configured with command-line flags (which can also be set through environment variables).

## Usage

```
hookmux [--listen :8080] [--config config.yml] [--timeout 30s] [--max-body 10485760] [log flags]
```
| Flag           | Environment variable | Default      | Description                                      |
| -------------- | -------------------- | ------------ | ------------------------------------------------ |
| `--listen`     | `LISTEN`             | `:8080`      | Address to listen for incoming webhooks on.      |
| `--config`     | `CONFIG`             | `config.yml` | Path to the YAML routing configuration.          |
| `--timeout`    | `TIMEOUT`            | `30s`        | Maximum time allowed per forward or exec action. |
| `--max-body`   | `MAX_BODY`           | `10485760`   | Maximum accepted webhook body size, in bytes.    |
| `--log.level`  | `LOG_LEVEL`          | `info`       | Log level: `debug`, `info`, `warn` or `error`.   |
| `--log.format` | `LOG_FORMAT`         | `text`       | Log format: `text` or `json`.                    |

Environment variable names are the flag name upper-cased with dashes replaced by underscores. Command-line flags take precedence over environment variables.

## Config

- [Routing configuration](docs/routes.md) — route fields, forward and exec actions
- [Matching](docs/matching.md) — jq payload expressions and path prefixes
- [Verifying webhooks](docs/verify.md) — signature schemes for GitHub, GitLab, Stripe and more