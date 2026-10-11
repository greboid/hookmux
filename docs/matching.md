# Matching

Two independent conditions decide whether a route fires: a `match` jq
expression evaluated against the payload, and an optional `matchpath` prefix
checked against the request path. When both are present, both must pass.

## Payload matching

`match` is a [jq](https://jqlang.github.io/jq/) expression evaluated against
the parsed JSON payload. The route matches if the expression returns anything
other than `false` or `null`. Omit it to match every webhook.

Tips:

- The payload is the jq input: `.event == "push"`, `.alerts | length > 0`,
  `.head_commit.id` all work as match expressions.
- `body` transforms on forward actions are ordinary jq expressions too:
  `{status: .status}` builds a new object; `[.alerts[].labels.alertname]`
  builds an array.
- Number precision is preserved (`json.Number`), so large IDs survive
  transforms intact.
- Non-JSON payloads never match a route that has a `match` expression.

## Path matching

A route with a `matchpath` only fires for requests sent to a matching path.
Each value is treated as a path prefix and compared at a path-segment
boundary: `matchpath: /github` matches `/github` and `/github/pushes`, but not
`/githubs` or `/gitlab`. A route may list several prefixes, in which case any
of them can match:

```yaml
routes:
  - name: forge-pushes
    matchpath: [/github, /gitea]
    match: '.event == "push"'
    forward:
      - url: https://ci.example.com/hooks/forge
```

Leading and trailing slashes are optional and normalised away, so `github` and
`/github/` both mean `/github`. Prefix matching never inspects the query
string, and works regardless of the payload — a `matchpath` route matches even
non-JSON payloads, as long as no `match` expression is also defined.
