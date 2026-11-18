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

To serve HTTP instead, set `TRANSPORT=http` and `PUBLIC_URL`. The MCP endpoint
then listens on `127.0.0.1:8081` and has **no authentication**: it is meant to
sit behind the resource-proxy sidecar, and refuses to listen anywhere but
loopback. **Read [authentication](#authentication) before you do.**

## 3. On Kubernetes, with the Helm chart

    helm install homebox-mcp oci://ghcr.io/excavador/charts/homebox-mcp \
      --version 0.3.0 \
      --set homebox.url=http://homebox \
      --set homebox.existingSecret=homebox-mcp \
      --set auth.issuerUrl=https://access.example \
      --set auth.resourceUrl=https://mcp.example/homebox \
      --set proxy.image.tag=<release> \
      --set proxy.image.digest=sha256:<index digest> \
      --set-json 'networkPolicy.ingressFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"<gateway namespace>"}}}]'

The chart always runs the HTTP transport; stdio in a pod with no attached
client exits immediately. It runs **two containers**: the server, and the
[sluis resource-proxy](https://github.com/truvity/sluis) that authenticates
callers in front of it (see [Authentication](#authentication)). Pin the proxy
release you have verified by tag **and** digest; the chart carries no default
version, and refuses to render without both.

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
| `auth.issuerUrl` | — | **Required.** The issuer's own URL, exactly as it appears in a token's `iss`. https. The proxy validates against it. |
| `auth.resourceUrl` | — | **Required.** This server's own EXTERNAL URL — the RFC 8707 audience the proxy requires. https, a path, no trailing slash. Must match this resource's id in the issuer's policy, byte for byte. Its path is where the gateway routes the server, and the base of upload links. |
| `auth.scope` | `openid` | Advertised in the PRM's `scopes_supported` and the 401 challenge's `scope`; never checked. Some issuers refuse an authorize request with no scope at all. Empty omits both fields. |
| `proxy.image.repository` | `ghcr.io/truvity/sluis/resource-proxy` | The sidecar image. |
| `proxy.image.tag`, `proxy.image.digest` | — | **Required.** The release (no leading `v`) and its image index digest, `sha256:<64 hex>`. |
| `proxy.maxRequestBytes` | `16777216` | The sidecar's body cap (`413` beyond it); its own default is 4 MiB. `data_base64` attachments need about 13.4 MB. |
| `proxy.resources` | 10m / 32Mi, limit 128Mi | The sidecar buffers a JSON body, so keep the limit several times `proxy.maxRequestBytes`. |
| `proxy.runAsUser` | `65532` | The image's uid, stated because `runAsNonRoot` cannot check a name. |
| `image.registry` / `image.repository` | `ghcr.io` / `excavador/homebox-mcp` | |
| `image.tag` | `""` | Empty means the chart's `appVersion`. Pin it to upgrade deliberately rather than whenever the chart is republished. |
| `replicaCount` | `1` | The server is stateless, so more than one is safe. |
| `service.port` | `8080` | The Service port that reaches the proxy. |
| `service.uploadPort` | `8082` | The Service port that reaches the upload listener, past the proxy. |
| `resources` | 20m / 64Mi, limit 128Mi | The server's own. |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, all capabilities dropped | Pod Security Standard `restricted`. The ServiceAccount token is not mounted. |
| `networkPolicy.enabled` | `true` | Ingress on the proxy and upload ports from `networkPolicy.ingressFrom` only; egress DNS, HTTPS and HomeBox. |
| `networkPolicy.ingressFrom` | `[]` | **Required while enabled.** The gateway's pods (an empty list would admit nobody, so the chart refuses it). |
| `networkPolicy.egress.https`, `.dns`, `.homebox.to` | `[]` | Peers to narrow egress to. Empty means any destination on the port. |
| `networkPolicy.egress.homebox.ports` | the port of `homebox.url` | Set it when HomeBox's container port differs from its Service's: a policy applies to the pod's port on a CNI that matches after the Service's translation. |
| `networkPolicy.extraEgress` | `[]` | Extra rules, verbatim. |
| `httpRoute.enabled` | `false` | Ship the HTTPRoute with the Service (see [the route](#the-route)). |
| `httpRoute.parentRefs` | `[]` | **Required while enabled.** The Gateway the route attaches to. |
| `httpRoute.hostnames`, `.name`, `.annotations`, `.labels` | | Optional. When `hostnames` is set it must include the host of `auth.resourceUrl`. |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |

The schema sets `additionalProperties: false`, so a typo like
`replicaCounts` fails to render instead of being silently ignored.

### Authentication

The server does not authenticate anything; the sidecar in the same pod does.
Every request to the sidecar's port needs a bearer token minted by
`auth.issuerUrl` for `auth.resourceUrl`: signature checked against the issuer's
JWKS, `iss` as configured, and `aud` equal to `auth.resourceUrl` exactly (RFC
8707). The sidecar also serves the protected-resource metadata, writes one
`mcp_request` audit line per request to its stdout (who, which method and
tool, the status; never a token, a query string or an argument), and strips the
caller's `Authorization` header before forwarding. If the issuer cannot be
reached it answers `503`, not `401`. See [design/cimd-auth.md](design/cimd-auth.md)
for the policy the issuer needs.

The pod:

    gateway ──▶ Service :8080 ──▶ proxy :8080 ──▶ 127.0.0.1:8081/mcp  server
            └─▶ Service :8082 ──▶ upload listener (pod IP :8082)      same server
                                   ops :9090 /healthz (kubelet only)

The chart refuses to render without `auth.issuerUrl`, `auth.resourceUrl` and
the sidecar's tag and digest. The Service lists the proxy port and the upload
port, and nothing else.

#### The route

A route (the chart's `httpRoute`, or your own) sends the resource's path to the
proxy's Service port with **no rewrite**: the proxy maps the path to the
server's `/mcp` itself and answers 404 for anything outside it. The one
exception is the signed upload link,
`PUT <auth.resourceUrl>/upload/<token>`: it carries no access token (a curl in
a shell has none; the link is the credential), so it goes straight to the
upload port, past the proxy. For `https://mcp.example/homebox`:

| path | goes to |
|---|---|
| `/homebox/upload/...` | Service `uploadPort` (8082), request timeout = `upload.timeout` |
| `/homebox`, `/homebox/...` | Service `port` (8080), no timeout |
| `/.well-known/oauth-protected-resource/homebox` and the bare `/.well-known/oauth-protected-resource` | Service `port`; the proxy serves the metadata |
| `/homebox/.well-known/oauth-protected-resource` | Service `port`, rewritten to the first form |

The longer prefix wins, so the upload rule takes `/homebox/upload`. The MCP rules
carry no timeout on purpose: a streamable response may stay open.

### Health

`/healthz` is on the server's ops port (9090) and answers liveness for the
server container; it is in no Service. It does **not** call HomeBox: a probe
that fails when a dependency blips would restart a pod for something restarting
cannot fix. The sidecar's `/healthz` is its liveness and its `/readyz` — which
waits for the issuer's discovery document — is the pod's readiness, so an issuer
outage takes the pod out of service rather than serve refusals.

## Configuration reference

Every setting is a flag or an environment variable; the chart sets the last
two itself.

| flag | env | default | |
|---|---|---|---|
| `--homebox-url` | `HOMEBOX_URL` | — | **Required.** Base URL, no `/api`. |
| `--homebox-token` | `HOMEBOX_TOKEN` | — | **Required.** HomeBox API key. |
| `--transport` | `TRANSPORT` | `stdio` | `stdio` or `http`. |
| `--addr` | `ADDR` | `127.0.0.1:8081` | HTTP transport only. The MCP endpoint, **loopback only**: any other address is refused at startup. |
| `--allow-non-loopback` | `ALLOW_NON_LOOPBACK` | `false` | HTTP transport only. Lets `--addr` bind a non-loopback address. The endpoint then has **no authentication**; use only where the network does the isolation. |
| `--upload-addr` | `UPLOAD_ADDR` | `127.0.0.1:8082` | HTTP transport only. Serves the signed upload link and nothing else. The chart sets `$(POD_IP):8082`. |
| `--ops-addr` | `OPS_ADDR` | `:9090` | HTTP transport only. Serves `/healthz` and nothing else. |
| `--public-url` | `PUBLIC_URL` | — | **Required for `http`.** This server's own external URL; the base of upload links, whose path the upload listener serves them at. Equal to the sidecar's `RESOURCE_URL`. |
| `--upload-max-bytes`, `--upload-ttl`, `--upload-timeout`, `--homebox-timeout` | `UPLOAD_MAX_BYTES`, `UPLOAD_TTL`, `UPLOAD_TIMEOUT`, `HOMEBOX_TIMEOUT` | see the chart | |

`ISSUER_URL`, `RESOURCE_URL` and `SCOPE` are **not** server settings any more:
set on the server they make it exit, because they would suggest it checks
tokens. They are the sidecar's.

## Verifying it works

Against the **sidecar's** port (the server's own endpoint is on loopback and
answers nothing outside the pod). Without a token, the MCP endpoint refuses:

    curl -s -i http://localhost:8080/homebox        # RESOURCE_URL's path

answers `401` with a
`WWW-Authenticate: Bearer resource_metadata="...", scope="openid"` header
naming the protected-resource metadata. Fetching that document (unauthenticated)
confirms the wiring:

    curl -s http://localhost:8080/.well-known/oauth-protected-resource/homebox

which answers `{"resource": "<RESOURCE_URL>", "authorization_servers":
["<ISSUER_URL>"], "scopes_supported": ["<SCOPE>"], ...}`.

With a real access token (minted by `auth.issuerUrl` for this server's
resource):

    curl -s -X POST http://localhost:8080/homebox \
      -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
           "protocolVersion":"2025-06-18","capabilities":{},
           "clientInfo":{"name":"probe","version":"1"}}}'

A successful initialize returns the server info and an `Mcp-Session-Id`
header, which subsequent requests must carry.

The signed upload link is the one path that must NOT need a token: a `PUT` of a
small file to a link from `homebox_prepare_attachment_upload` answers `201`,
through the route and past the proxy. From a pod other than the gateway's, the
server's port 8081 must refuse the connection.

Startup failure reports `homebox unreachable or token rejected` — the two
are not distinguished at the top level, but the wrapped error is, so read the
whole line: a network error names the host, while a rejected key shows
`401 Unauthorized` from `/users/self`.
