# wuko-plugin-http

`wuko-plugin-http` implements Wuko plugin protocol v2 and contributes two lifecycle-managed HTTP
testing services:

- `http.forward_proxy` intercepts and rewrites opted-in HTTP and HTTPS traffic.
- `http.mock_server` serves stateful expectations and verifies them when its scope ends.

Install it from the marketplace before running a workflow that uses either type:

```sh
wuko plugin install --global --package http https://github.com/up2jj/wuko-marketplace
```

Both services default to an available loopback port. Wuko supervises them after readiness and shuts
them down with their containing workflow scope.

## `http.forward_proxy`

Start a lifecycle-managed, loopback-only forward proxy that can inspect and rewrite opted-in HTTP
and HTTPS requests. It uses Go's HTTP and TLS libraries directly. HTTPS uses a per-run certificate
authority; its private key remains in memory and the generated CA certificate is removed during
workflow cleanup.

```yaml
- id: proxy
  type: http.forward_proxy
  with:
    # Optional; defaults to an available loopback port at 127.0.0.1:0.
    listen: 127.0.0.1:0
    keep_alive: false
    max_body_bytes: 10MiB
    rules:
      - name: redirect_api
        when: 'original.method == "POST" && original.host == "api.example.com"'
        rewrite:
          method: PUT
          host: staging.example.com
          path: /v2/items
          headers:
            set: {X-Proxied: ["true"]}
            add: {X-Trace: ["wuko"]}
            remove: [Authorization]
          query:
            set: {source: ["wuko"]}
            add: {tag: ["intercepted"]}
            remove: [token]
          body: '{"rewritten":true}'
        stop: false
    log:
      destination: stdout
      include_bodies: false
      redact_headers: [X-Private]
```

Rules evaluate `when` against the immutable `original` request. Its fields are `method`, `url`,
`scheme`, `host`, `path`, `query`, `headers`, `body`, and `body_base64`. Every matching rule is
applied in declaration order; `stop: true` prevents later rules from running. A rewrite may set
`url`, or individual `scheme`, `host`, and `path` fields; these forms are mutually exclusive. It
may also change the method, set/add/remove header and query values, and replace either `body` or
`body_base64`.

An optional isolated Lua transform runs after declarative rules. Each request gets a fresh state
with only the base, table, string, and math libraries:

```yaml
lua:
  source: |
    function handle(original, request, matched_rules)
      request.headers["X-Original-Host"] = {original.host}
      return request -- nil preserves the current request
    end
```

Transformation errors affect only that request and return `502`; the proxy continues serving.
Request bodies are buffered up to `max_body_bytes` (exactly 10 MiB by default), with larger bodies
returning `413`. Responses are not transformed and are streamed back to the client. CONNECT is
used only to intercept HTTPS, not as a general TCP tunnel. The proxy is deliberately local,
unauthenticated, does not inherit environment proxy settings, and cannot run inside an executor.

Only proxy-form traffic is served. A request sent straight at the listener rather than through a
client's proxy setting returns `400`, and a rewrite that points a request back at the proxy's own
address returns `502` -- either would otherwise make the proxy call itself in a loop. The query
string is passed through byte for byte unless a rule's `query` block or the Lua transform changes
it; a changed query is re-encoded, which sorts its keys.

The outputs are `ready`, `url`, `ca_cert`, and the SHA-256 fingerprint `ca_sha256`. When file
logging is selected, `log_path` is also returned. Use those outputs with Wuko's `http` step:

```yaml
- id: call
  type: http
  with:
    url: https://api.example.com/items
    proxy: {url: "{{ .steps.proxy.url }}"}
    root_ca_file: "{{ .steps.proxy.ca_cert }}"
```

Omit `log` to disable traffic logging. `destination: stdout` writes one serialized JSONL record per
request through the step output stream; Wuko's own progress remains on stderr. Other concurrent
steps can still write between complete proxy records, so use `destination: file` for a standalone
machine-readable log:

```yaml
log:
  destination: file
  path: .wuko/proxy.jsonl
  overwrite: false
  include_bodies: false
```

File logs use mode `0600` and reject an existing path unless `overwrite` is true. Authorization,
proxy authorization, cookies, common secret query parameters, and configured `redact_headers` are
redacted. Bodies are omitted unless `include_bodies` is enabled.

## `http.mock_server`

Start a lifecycle-managed, loopback-only origin server from expectation files. `when` selects the
first eligible expectation; `assertions` validate the selected request. The server keeps serving
after request-local failures and verifies all traffic and exact `times` counts when its containing
scope ends.

```yaml
- id: mock
  type: http.mock_server
  with:
    expectations:
      - mocks/common
      - mocks/users/**/*.yaml
      - mocks/uploads
      - mocks/special.yml
    initial_state: {users: {}}
    max_body_bytes: 10485760
    log: {destination: file, path: mock-traffic.jsonl, overwrite: true}
```

Expectation sources are processed in declaration order. A source may be an exact `.yaml`/`.yml`
file, a recursively scanned directory, or a doublestar glob. Files within each source are sorted
lexically and the first occurrence wins when sources overlap. Sources and response/state files
must remain inside the packaged workflow or action, and YAML symlinks are rejected.

Each expectation file is strict and versioned:

```yaml
version: 1

templates:
  headers:
    json_response:
      Content-Type: application/json
  bodies:
    user:
      json: {expr: "state.users[request.params.user_id]"}

expectations:
  - name: store_user
    times: 1
    when: |
      request.method == "PUT" &&
      route("/users/{user_id}")
    assertions:
      - name: authorized
        expr: 'header("Authorization") == "Bearer " + vars.mock_token'
        message: Expected the configured mock bearer token
      - name: valid_name
        expr: 'jsonBody().name != ""'
      - name: supported_role
        expr: 'request.json.role in ["admin", "member"]'
        message: 'Unsupported role {{ .request.json.role }}'
    update:
      - op: set
        path: '/users/{{ jsonPointerEscape .request.params.user_id }}'
        value: {expr: request.json}
    respond:
      status: 201
      headers_template: json_response
      body_template: user
```

Names must be unique across all loaded files. Omitted `times` is unlimited; a supplied positive
value is exact. Expectations whose count is already satisfied are skipped. A matched request runs
every assertion so the `422` response contains the complete failure list:

```json
{
  "error": "mock request assertion failed",
  "expectation": "store_user",
  "assertions": [{"name": "supported_role", "message": "Unsupported role guest"}]
}
```

False expressions and evaluation errors are both failures and are distinguished in shutdown
diagnostics. Assertion messages are request-aware Go templates, validated at startup and rendered
only on failure. Failed assertions do not run updates, render the configured response, or consume
`times`. They are retained for final verification, as are malformed, unmatched, processing, and
logging failures. Shutdown diagnostics retain the total and details for the first 20 failed
requests. Custom final-state assertions are not supported; use exact `times` for shutdown-level
verification.

`request` exposes these Expr fields:

| Field | Value |
| --- | --- |
| `method`, `url`, `scheme`, `host`, `path` | Normalized request target |
| `headers` | All header values by name |
| `body`, `body_base64` | Raw body as text, and its base64 form when an expectation, a `body_file`, or a body-recording log reads it |
| `params` | Captures produced by `route()` |
| `query`, `query_all` | First and all query values |
| `json` | Parsed JSON value when valid |
| `form`, `form_all` | First and all multipart text fields |
| `files` | Multipart uploads grouped by field |

An uploaded file has `filename`, `content_type`, `size`, and `body_base64`; `body` is also present
when its bytes are valid UTF-8. The helpers `route(pattern)`, `header(name)`, `query(name)`,
`form(name)`, and `jsonBody()` provide concise matching and validation. `jsonBody()` fails
evaluation when JSON is unavailable. Multipart requests are decoded in memory within
`max_body_bytes`; malformed multipart returns `400`, changes no state or counts, and fails final
verification. URL-encoded form bodies are not decoded.

```yaml
- name: upload_avatar
  when: 'request.method == "POST" && route("/users/{user_id}/avatar")'
  assertions:
    - {name: category, expr: 'form("category") == "profile"'}
    - {name: one_avatar, expr: 'len(request.files.avatar) == 1'}
    - {name: png_file, expr: 'request.files.avatar[0].content_type == "image/png"'}
    - name: file_size
      expr: 'request.files.avatar[0].size <= vars.max_avatar_bytes'
      message: Avatar exceeds the configured size limit
  respond:
    status: 201
    json:
      filename: {expr: 'request.files.avatar[0].filename'}
      size: {expr: 'request.files.avatar[0].size'}
```

Expressions can also read pre-request `state` and frozen Wuko roots captured at server start:
`vars`, `inputs`, `env`, `steps`, `dependencies`, providers, secrets, and plugin helpers. String
values in responses and patches are Go templates with the same frozen roots plus `.request` and
`.state`. Response construction sees post-update state; routing, assertions, and updates see
pre-request state.

State starts as `{}`, an inline object, or a confined YAML/JSON `initial_state_file`. An inline
`initial_state` is rendered once with the rest of the step configuration, so its result is data;
only `initial_state_file` content is templated by the step itself. Updates are ordered RFC 6901
patches:

```yaml
update:
  - {op: set, path: /users/alice, value: {expr: request.json}}
  - {op: append, path: /events, value: {expr: request.json}}
  - {op: remove, path: /pending/alice}
```

A lone `{literal: ...}` in a `json` body or a patch `value` is taken verbatim: nothing inside it is
compiled or rendered. Use `jsonPointerEscape` when a request value becomes one path token. Requests are serialized so
concurrent clients observe deterministic, atomic state transitions. State and count changes commit
only after all updates and response construction succeed.

Scalar `respond` is a templated status-`200` body. Object responses accept static `status` and
header names, templated `headers`, and exactly one body form:

| Field | Meaning |
| --- | --- |
| `body` | Templated text |
| `literal_body` | Exact, unrendered text |
| `json` | JSON tree; `{expr: "..."}` leaves preserve native types, `{literal: ...}` escapes a payload that carries its own `expr` key or template delimiters |
| `body_file` | Confined UTF-8 file local to the expectation file, rendered as a template |
| `body_base64` | Templated base64 decoded to response bytes |

File-local `headers_template` and `body_template` names select reusable declarations from the
file's `templates` block. Explicit headers replace same-named template headers. An explicit body
replaces a named body, except JSON objects are recursively overlaid.

Unmatched requests return `404`; malformed requests return `400`; assertion failures return `422`;
matching, patch, or response failures return `500`. None uses an expectation's configured response,
and the server continues accepting requests. A client that hangs up mid-response is recorded in the
traffic log but does not fail the step: the mock served the request, and the timeout belongs to the
caller.

Set `tls: true` for generated HTTPS. Outputs are `ready` and `url`, plus `ca_cert` and
`ca_sha256` for HTTPS and `log_path` for file logging:

```yaml
- id: mock
  type: http.mock_server
  with: {tls: true, expectations: [mocks]}
- id: call
  type: http
  with:
    url: '{{ .steps.mock.url }}/users/alice'
    root_ca_file: '{{ .steps.mock.ca_cert }}'
```

The generated CA certificate is private (`0600`) and removed during cleanup; its private key stays
in memory. JSONL logging uses the same private file behavior. Authorization and cookie headers,
common secret query fields, and configured `redact_headers` are redacted. Bodies are omitted unless
`include_bodies: true`; decoded JSON, form fields, upload bodies, and state snapshots are never
logged separately.

## Development and release

The module is pinned to the Wuko commit that introduced protocol v2. Until that commit is available
from the module proxy, develop it in a Go workspace containing both repositories.

```sh
just check
just build
just release 0.1.0
```

From the marketplace root, import the release and rebuild the catalog:

```sh
wuko marketplace plugin add \
  --description "Intercept HTTP traffic and run stateful verified mock servers" \
  ./plugin-src/http
wuko marketplace build
wuko marketplace build --check
```

