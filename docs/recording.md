# Recording payloads

Hookmux can write every webhook it accepts to disk so you can inspect what senders actually delivered, or replay problematic payloads later. Recording is controlled by two flags (also settable as environment variables):

| Flag           | Values                          | Behaviour                                             |
| -------------- | ------------------------------- | ----------------------------------------------------- |
| `--record`     | `none` (default)                | Nothing is recorded.                                  |
|                | `unhandled`                     | Only payloads that matched no route are recorded.     |
|                | `all`                           | Every accepted payload is recorded.                   |
| `--record-dir` | any directory (default `recordings`) | Created on startup if it does not exist.         |

A payload counts as unhandled when no route matched it — not even a `noop` route. Once any route matches (even one that only runs a `noop`), the payload is considered handled.

## File format

Each webhook becomes one JSON file named `<timestamp>-<sequence>.json`:

```json
{
  "timestamp": "2025-10-11T12:00:00.123456789+01:00",
  "method": "POST",
  "uri": "/github?run=1",
  "remote": "140.82.115.250:44116",
  "headers": {
    "Content-Type": ["application/json"],
    "X-Hub-Signature-256": ["sha256=..."]
  },
  "matched": ["github-pushes"],
  "body": "{\"event\":\"push\"}"
}
```

- `matched` lists the routes that fired; it is empty for unhandled payloads.
- The body is stored as a string. Non-UTF-8 bodies are base64-encoded with `"body_base64": true`.

Files are written with mode `0600` because headers often carry signatures or tokens. Treat the directory as sensitive.

Recording happens synchronously before actions are dispatched, and failures are logged rather than fatal: recording never changes webhook handling or the `202 accepted` response.
