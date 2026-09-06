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

To serve HTTP instead, set `TRANSPORT=http` and publish the port. **Read
[the warning](#the-server-has-no-authentication) before you do.**

## 3. On Kubernetes, with the Helm chart

    helm install homebox-mcp oci://ghcr.io/excavador/charts/homebox-mcp \
      --version 0.3.0 \
      --set homebox.url=http://homebox \
      --set homebox.existingSecret=homebox-mcp

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
| `image.registry` / `image.repository` | `ghcr.io` / `excavador/homebox-mcp` | |
| `image.tag` | `""` | Empty means the chart's `appVersion`. Pin it to upgrade deliberately rather than whenever the chart is republished. |
| `replicaCount` | `1` | The server is stateless, so more than one is safe. |
| `service.port` | `8080` | Also the container's listen port. |
| `resources` | 20m / 64Mi, limit 128Mi | |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs, all capabilities dropped | |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |

The schema sets `additionalProperties: false`, so a typo like
`replicaCounts` fails to render instead of being silently ignored.

### The server has no authentication

The chart deliberately offers nothing that turns authentication on, because
the server has none. It expects a gateway in front of it that validates a
token.

A `Service` of type `ClusterIP` and no Ingress or HTTPRoute is the safe
default the chart ships. **Do not expose it** until something in front is
checking credentials. On hive that is Envoy Gateway with a `SecurityPolicy`
validating a Zitadel-issued JWT against the `homebox` project audience; the
route and policy live in the cluster repo, not in this chart, because they
are site-specific.

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

## Verifying it works

Over HTTP, the MCP endpoint is `/mcp`:

    curl -s -X POST http://localhost:8080/mcp \
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
