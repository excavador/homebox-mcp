# Installation

Three ways to run it, in rough order of how most people will: a binary on
your own machine, a container, and the Helm chart for a cluster.

Whichever you choose, you need a HomeBox API key first.

## Getting an API key

HomeBox accepts exactly one credential — its own API key. It does **not**
accept an OIDC token; HomeBox's OIDC support is a browser login flow that
ends by issuing one of these keys.

1. Log into HomeBox in a browser.
2. Profile → **Settings** → **API Keys** → create one.
3. Copy it. It is shown once, and it looks like `hb_...`.

The key inherits its owner's **group**, and HomeBox scopes data by group
rather than by user. Everyone in one group sees one inventory. For a
household that is the point; if you need per-person visibility, this server
cannot give it to you — see
[one shared identity](architecture.md#one-shared-identity-and-what-that-costs).

If your HomeBox has local login disabled, the browser is still the only way
to mint the first key — the API cannot bootstrap itself.

## 1. As a local binary

Download a release for your platform from
[Releases](https://github.com/excavador/homebox-mcp/releases), or build it:

    go build ./cmd/homebox-mcp

Run it over stdio, which is the default and what a local MCP client expects:

    HOMEBOX_URL=https://homebox.example.com \
    HOMEBOX_TOKEN=hb_... \
    ./homebox-mcp

There is no output on success — stdio transport means the protocol owns
stdout. Startup logs go to stderr; you should see one line naming the
authenticated user. If the token is wrong it fails immediately and says so.

Then point a client at it — see [clients](clients.md).

## 2. As a container

    docker run --rm -i \
      -e HOMEBOX_URL=https://homebox.example.com \
      -e HOMEBOX_TOKEN=hb_... \
      ghcr.io/excavador/homebox-mcp:0.3.0

The image is multi-arch (`linux/amd64`, `linux/arm64`) and built with ko from
a distroless static base: no shell, no package manager, runs as non-root.

To serve HTTP instead, set `TRANSPORT=http` plus `ISSUER_URL` and
`RESOURCE_URL`, and publish the port. **Read
[authentication](#authentication) before you do.**

## 3. On Kubernetes, with the Helm chart

    helm install homebox-mcp oci://ghcr.io/excavador/charts/homebox-mcp \
      --version 0.3.0 \
      --set homebox.url=http://homebox \
      --set homebox.existingSecret=homebox-mcp \
      --set auth.issuerUrl=https://access.example \
      --set auth.resourceUrl=https://mcp.example/homebox

The chart always runs the HTTP transport; stdio in a pod with no attached
client exits immediately.

### The token comes from a Secret, always

There is no value that takes the token as a literal. A literal would end up
in a values file, in git, and in `helm get values` output. The chart reads it
from a Secret you provide:

    kubectl create secret generic homebox-mcp --from-literal=token=hb_...

Or, with External Secrets Operator, keep it in a secret store:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: homebox-mcp
spec:
  refreshInterval: 1h
  secretStoreRef: {name: aws-parameter-store, kind: ClusterSecretStore}
  target: {name: homebox-mcp}
  data:
    - secretKey: token
      remoteRef: {key: /homebox/mcp-api-key}
```

### Values

| value | default | notes |
|---|---|---|
| `homebox.url` | — | **Required.** Base URL **without** `/api`; the server appends `/api/v1`. The chart refuses a URL ending in `/api`. |
| `homebox.existingSecret` | — | **Required.** Secret holding the API key. |
| `homebox.existingSecretTokenKey` | `token` | Key within that Secret. |
| `auth.issuerUrl` | — | **Required.** access-roster's own URL, exactly as it appears in a token's `iss`. |
| `auth.resourceUrl` | — | **Required.** This server's own EXTERNAL URL — the RFC 8707 audience access-roster mints tokens for. Must match this resource's id in access-roster's policy, byte for byte. |
| `auth.scope` | `openid` | Advertised in the PRM's `scopes_supported` and the 401 challenge's `scope`; never checked by this server. access-roster refuses an authorize request with no scope at all, so this exists for a scope-less client to adopt. An estate needing more sets this itself; empty omits both fields. |
| `image.registry` / `image.repository` | `ghcr.io` / `excavador/homebox-mcp` | |
| `image.tag` | `""` | Empty means the chart's `appVersion`. Pin it to upgrade deliberately rather than whenever the chart is republished. |
| `replicaCount` | `1` | The server is stateless, so more than one is safe. |
| `service.port` | `8080` | Also the container's listen port. |
| `resources` | 20m / 64Mi, limit 128Mi | |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, all capabilities dropped | |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |

The schema sets `additionalProperties: false`, so a typo like
`replicaCounts` fails to render instead of being silently ignored.

### Authentication

The server validates every request itself against access-roster: a bearer
token, checked against `auth.issuerUrl`'s JWKS, with `aud` required to
equal `auth.resourceUrl` exactly (RFC 8707). Both values are required —
there is no way to render the chart, or start the http transport directly,
without them, and no gateway fallback any more. See
[design/cimd-auth.md](design/cimd-auth.md) for the full design and the
exact access-roster policy this expects.

A `Service` of type `ClusterIP` and no Ingress or HTTPRoute is the safe
default the chart ships regardless — routing and TLS termination are
still a deployment's own concern (a Gateway, an Ingress, a plain
port-forward), because those are site-specific. What changed is that
whatever fronts the Service no longer needs to be the thing that decides
who may call it.

### Health

`/healthz` answers liveness and readiness. It does **not** call HomeBox: a
readiness probe that fails when a dependency blips would take the pod out of
service for something restarting cannot fix.

## Configuration reference

Every setting is a flag or an environment variable; the chart sets the last
two itself.

| flag | env | default | |
|---|---|---|---|
| `--homebox-url` | `HOMEBOX_URL` | — | **Required.** Base URL, no `/api`. |
| `--homebox-token` | `HOMEBOX_TOKEN` | — | **Required.** HomeBox API key. |
| `--transport` | `TRANSPORT` | `stdio` | `stdio` or `http`. |
| `--addr` | `ADDR` | `0.0.0.0:8080` | HTTP transport only. |
| `--issuer-url` | `ISSUER_URL` | — | **Required for `http`.** access-roster's own URL. |
| `--resource-url` | `RESOURCE_URL` | — | **Required for `http`.** This server's own external URL (RFC 8707 audience). |
| `--scope` | `SCOPE` | `openid` | HTTP transport only. Advertised in the PRM and the 401 challenge; never checked here. |

## Verifying it works

Without a token, the MCP endpoint refuses:

    curl -s -i http://localhost:8080/mcp

answers `401` with a
`WWW-Authenticate: Bearer resource_metadata="...", scope="openid"` header
naming this server's own protected-resource metadata. Fetching that
document (unauthenticated) confirms the wiring:

    curl -s http://localhost:8080/.well-known/oauth-protected-resource

which answers `{"resource": "<RESOURCE_URL>", "authorization_servers":
["<ISSUER_URL>"], "scopes_supported": ["<SCOPE>"], ...}`.

With a real access token (minted by `auth.issuerUrl` for this server's
resource):

    curl -s -X POST http://localhost:8080/mcp \
      -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
           "protocolVersion":"2025-06-18","capabilities":{},
           "clientInfo":{"name":"probe","version":"1"}}}'

A successful initialize returns the server info and an `Mcp-Session-Id`
header, which subsequent requests must carry.

Startup failure reports `homebox unreachable or token rejected` — the two
are not distinguished at the top level, but the wrapped error is, so read the
whole line: a network error names the host, while a rejected key shows
`401 Unauthorized` from `/users/self`.
