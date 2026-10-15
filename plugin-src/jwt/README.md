# Wuko JWT plugin

`jwt` adds dependency-free RS256 token generation, public JWKS projection, and strict JWT
verification to Wuko. It uses the Wuko Go SDK and plugin protocol v3, requires Wuko 0.16.0 or
newer, and exposes the same operations as workflow steps and helpers.

## Install

```sh
wuko plugin install --global --package jwt https://github.com/up2jj/wuko-marketplace
```

Installed plugins provide steps immediately. Helpers must be pinned in the workflow so Wuko can
build the helper registry before evaluation:

```yaml
plugins:
  jwt:
    source: github:up2jj/wuko-marketplace@jwt-v0.2.0:plugins/jwt/plugin.json
    sha256: e633ff3fc70ff7d1bca93acf05e885d97ae944866cbb7c164e0f135405e0a083
```

## Steps

Generate a token. `iat`, `exp`, and a random `jti` are supplied by default; entries in `claims`
override defaults and registered values supplied as options.

```yaml
- id: issue
  type: jwt.generate
  with:
    issuer: https://issuer.example
    audience: [orders-api, audit-api]
    subject: user-42
    ttl: 10m
    claims:
      role: admin

- id: verify
  type: jwt.verify
  with:
    token: "{{ .steps.issue.token }}"
    jwks: '{{ .steps.issue.jwks | toJSONCompact }}'
    issuer: https://issuer.example
    audience: orders-api
```

`jwt.generate` outputs `token`, `header`, `claims`, and the public `jwks` for its signing key.
`jwt.verify` outputs trusted `header` and `claims` and fails for an invalid token. `jwt.is_valid`
accepts the same fields and outputs `valid: false` for malformed, badly signed, expired, or
claim-invalid tokens. Configuration, network, and JWKS failures still fail the step.

`jwt.jwks` outputs the command-scoped public key when called without `signing_jwk`, or strips
private material from a supplied RSA JWK/JWKS:

```yaml
- id: public_keys
  type: jwt.jwks
  with:
    signing_jwk: '{{ secret("op://Engineering/JWT/private-jwk") }}'
```

Pass the same `signing_jwk` to `jwt.generate` to sign with caller-owned key material. It may be a
JWK/JWKS object or a JSON string; `claims` likewise accepts an object or a JSON-encoded object string. A multi-key set requires `kid`; a single key without `kid` gets
an RFC 7638 SHA-256 thumbprint. Private key material is never included in outputs or errors.

Verify against a remote JWKS with `jwks_url`. HTTPS is required except for loopback HTTP. Fetches
time out after five seconds, limit responses to 1 MiB, validate redirects, honor bounded cache
headers, and refresh once when a token references an unknown `kid`.

```yaml
- id: verify_remote
  type: jwt.verify
  with:
    token: "{{ .vars.token }}"
    jwks_url: https://issuer.example/.well-known/jwks.json
    issuer: https://issuer.example
    audience: orders-api
    leeway: 30s
    required_claims: [exp, iat, sub]
```

## Helpers

The helper names are `jwt_generate`, `jwt_jwks`, `jwt_verify`, and `jwt_is_valid`:

```yaml
- id: expr_token
  type: set
  with:
    variable: expr_token
    expr: 'jwt_generate({"sub":"user-42","role":"admin"}, {"ttl":"10m"})'

- id: template_token
  type: set
  with:
    variable: template_token
    value: '{{ jwt_generate (dict "sub" "user-42") (dict "ttl" "10m") }}'

- id: lua_token
  type: lua
  with:
    source: |
      local token = wuko.helpers.jwt_generate({sub = "user-42"}, {ttl = "10m"})
      local verified = wuko.helpers.jwt_verify(token)
      wuko.output("token", token)
      wuko.output("subject", verified.claims.sub)
```

Signatures are RS256 only. Verification requires `exp` by default and always validates any
present `exp`, `nbf`, and `iat`. Set `required_claims: []` explicitly to allow a token without
`exp`; this does not disable validation of an `exp` that is present.

## Development

From the marketplace root:

```sh
wuko marketplace plugin release --version 0.2.0 ./plugin-src/jwt
wuko marketplace build --check
```

The conventional workflow runs the tests, then cross-compiles deterministic Darwin/Linux archives
for amd64 and arm64 with CGO disabled.
