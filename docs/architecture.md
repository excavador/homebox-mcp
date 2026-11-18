# Architecture

A single Go binary that translates MCP tool calls into HomeBox REST calls.
There is no database, no cache and no state: every tool call is one HTTP
request to HomeBox, and the process can be restarted or scaled at will.

## The request path

    Claude Code (or any MCP client)
      │  presents client_id = an HTTPS URL to its own Client ID Metadata
      │  Document (CIMD) -- no pre-registration, no DCR
      │  gets back an access token: iss=the issuer, aud=this resource
      │  MCP over streamable HTTP, Authorization: Bearer <access token>
      ▼
    gateway  (routes the resource's path; rewrites nothing)
      ▼
    proxy container  (sluis resource-proxy, :8080)
      │  validates the token: signature against the issuer's JWKS, iss, and
      │  aud == the resource URL (RFC 8707); serves the RFC 9728 metadata;
      │  writes one audit line; strips the Authorization header; maps the
      │  resource's path to /mcp
      ▼
    homebox-mcp  (TRANSPORT=http, MCP on 127.0.0.1:8081/mcp)
      │  Authorization: Bearer <HomeBox API key>
      ▼
    HomeBox  /api/v1/...

Two different credentials, and conflating them is the mistake this design
exists to prevent:

- The **caller's** token is an access token identifying a person, bound to this
  server's own resource URL as its audience. The **proxy** verifies it, and
  `homebox-mcp` never sees it -- see [design/cimd-auth.md](design/cimd-auth.md).
- The **server's** token is a HomeBox API key, held in a Kubernetes Secret and
  read once at startup. HomeBox accepts nothing else — its OIDC support is a
  browser redirect flow that ends by *issuing* one of these keys.

### Where authentication lives

Not in this binary. The HTTP transport holds no authentication code and does
not import the issuer's library; the sluis `resource-proxy` sidecar in the same
pod is the pod's only authenticated port. Everything about *who may call* --
the issuer, the audience, the metadata, the audit line -- is the sidecar's
configuration (`ISSUER_URL`, `RESOURCE_URL`, `SCOPE`), and who may reach the
resource is the issuer's policy for that URL.

The server is built so that leaving the sidecar out cannot go unnoticed:

- **The MCP endpoint is loopback only.** `--addr` defaults to `127.0.0.1:8081`
  and the server **refuses to start** with any other address (`0.0.0.0`, an
  empty host, a pod IP, a name other than `localhost`), unless
  `--allow-non-loopback` / `ALLOW_NON_LOOPBACK=true` says the network around it
  does the isolation. A stray `0.0.0.0` in a values file therefore cannot
  publish the inventory. In a pod, loopback is reachable only by the other
  containers of the pod: the sidecar.
- **No port for it.** The chart declares no container port for the MCP
  endpoint, and the Service lists only the proxy port and the upload port.
- **A leftover auth setting is refused.** `ISSUER_URL`, `RESOURCE_URL` or
  `SCOPE` set on the *server* means someone still believes it checks tokens;
  it exits with a message saying where they belong now.
- **The chart will not render** without `auth.issuerUrl`, `auth.resourceUrl`
  (https, a path, no trailing slash) and a sidecar pinned by tag and digest.

### Three listeners

| listener | flag | default | serves | reached by |
|---|---|---|---|---|
| MCP | `--addr` | `127.0.0.1:8081` | `/mcp`, `/mcp/` | the sidecar, over loopback |
| upload | `--upload-addr` | `127.0.0.1:8082` (the chart: the pod IP) | `PUT <path of PUBLIC_URL>/upload/{token}` and **nothing else** | the gateway, past the sidecar |
| ops | `--ops-addr` | `:9090` | `GET /healthz` and nothing else | the kubelet |

Each handler is the whole of its listener: a path that does not belong is a
404, tested for each. `/healthz` is on the ops port so the Service can never
expose it, and the metadata is the sidecar's.

### One shared identity, and what that costs

Every caller the issuer admits is still one HomeBox API key as far as
HomeBox itself is concerned: the server makes every request with it, so
HomeBox attributes every change to that key rather than to the person who
asked for it. **HomeBox's audit trail cannot tell one user's deletion from
another's** -- the issuer identifies *who may reach this server*, not who
HomeBox believes made a given change.

That is accepted for a household whose users already share a HomeBox group
and can already edit everything through the browser. It is not acceptable
where per-user permissions or an audit trail matter, and fixing it properly
would need the server to exchange the caller's token for a per-user HomeBox
credential — which HomeBox cannot currently issue.

## Package layout

    cmd/homebox-mcp     flags, env, transport selection, signal handling
    internal/homebox    the HTTP client: auth, paths, error and body handling
    internal/server     tool registration; the MCP surface

`internal/server` never builds a URL and `internal/homebox` never knows what
a tool is. That split is what makes it possible to test the tools against a
stub HomeBox without a real one.

### Nothing is modelled

The client passes request and response bodies through as `any`. No structs
mirror HomeBox's types.

This is a deliberate trade. HomeBox's shapes are what its own docs and
swagger describe; a duplicate set of Go structs would add a second source of
truth that silently drops fields the upstream adds, and drift is invisible —
a missing field looks like a HomeBox that did not return it. Passing bodies
through means a HomeBox upgrade that adds a field exposes it immediately.

The cost is that a malformed body is only rejected by HomeBox, not by the
tool. Given the caller is a language model reading tool descriptions, an
error from HomeBox naming the real problem beats a schema error naming a
Go type.

## Three things that are load-bearing

### structuredContent must be an object

MCP requires the structured result to be a JSON object. Several HomeBox
endpoints answer with a top-level array — `/tags`, `/entities/tree`,
`/entities/{id}/path`, `/entities/fields` — and returning one straight
through makes the client reject the response as malformed, with an error
that names schema validation rather than the tool that produced it.

Every non-object response is wrapped: `{"items": [...]}`. This shipped
broken in v0.1.2, when four of eight tools were unusable and the two that
were tested happened to be the two object-shaped ones.

### Annotations are how a client decides what to ask about

Every tool carries MCP `ToolAnnotations`: `readOnlyHint` on reads,
`destructiveHint: false` on creates, `destructiveHint: true` on updates and
deletes. A client uses these to decide what it may call without confirming
first, so an unannotated tool is not a cosmetic omission — it defaults to the
cautious reading and silently loses function.

A test enumerates the registered tools and fails on any that is neither
read-only nor destructive-hinted.

### The write tools paper over three HomeBox behaviours

Left alone, all three lose data while reporting success -- the worst failure
mode a tool can have, because nothing downstream can tell.

**Create keeps three fields.** `POST /entities` applies `name`,
`entityTypeId` and `parentId`, discards the rest of the body, and answers 201.
`homebox_create_entity` follows it with the `PUT` that HomeBox does honour, so
the whole body lands. The second write is skipped when the body holds nothing
that would be dropped.

**Reads give objects, writes take ids.** An entity comes back with `parent`,
`entityType` and `tags` as nested objects; a write accepts `parentId`,
`entityTypeId` and `tagIds`. A `PUT` that omits an id form clears that
relation. So the obvious read-modify-write -- fetch, change one field, send it
back -- unparents the entity and strips its tags, silently, because none of
the id fields were in what you read.

Both write tools translate: for every relation the caller did not set
explicitly, the id is taken from the read shape, in the body if it is there
and from the entity otherwise. Passing `parentId` or `tagIds` still overrides,
so deliberate moves and retagging work.

**PATCH does nothing.** `PATCH /entities/{id}` answers 200 with the unchanged
entity: accepted, not written. `homebox_patch_entity` reads, merges and `PUT`s
instead, which is what a partial update means.

These were all found by loading a real inventory through the tools and then
checking the result, not by reading the API docs. Each is covered by a test
that drives the registered tool against a stub reproducing the quirk.

### The upload link is the one request that carries no bearer token

A file on the caller's machine cannot reach a server in a cluster through a
tool call without passing through the model. So `homebox_prepare_attachment_upload`
returns a link, and a `curl` in a shell sends the file straight to the server.
A shell has no access to the MCP session's bearer token, so the link itself is
the credential, and everything is done to make it worth little:

- **Signed, stateless.** An HMAC over the claims, keyed from a value derived
  from the HomeBox API key (not the key itself). The server keeps no list of
  links, so a restart or a second replica does not lose any.
- **Narrow.** One entity, one attachment type, one size cap, ten minutes.
  Editing a claim breaks the signature; every failure answers the same
  "invalid or expired" so a probe learns nothing about why.
- **Used once, but only when it worked.** The nonce is reserved when a PUT
  arrives, so two in parallel cannot both store a file, and released when the
  upload fails, so sending the wrong file does not cost the caller the link.
  The record is in memory and per replica; with several replicas a link could
  be replayed once on each, inside its ten minutes, to the same entity.
- **Checked like any other upload.** The type is sniffed from the first bytes,
  the declared type is not trusted, the extension is forced to the sniffed
  type, and the same concurrency cap applies. The size limit is enforced
  while reading, not from the declared length, which a sender can omit or lie
  about.
- **Streamed, so memory is flat.** Only the first 512 bytes are held; the rest
  goes through a pipe into the multipart request to HomeBox. Buffering a file
  twice, as the 10 MiB tool paths do, would need 200 MiB for a 100 MiB upload
  in a pod limited to 128 MiB. The whole upload has one deadline
  (`UPLOAD_TIMEOUT`), set on the read side too, so a sender that trickles
  bytes cannot hold a slot; the ordinary 30-second HomeBox timeout does not
  apply to it, because it counts the body.

The link is `PUT <PUBLIC_URL>/upload/{token}` and is served by the **upload
listener**, which is not behind the sidecar -- the sidecar would answer a
request with no access token `401` -- and carries nothing but this endpoint.
It serves the link at the path the client was given (the path of `PUBLIC_URL`,
`/homebox/upload/{token}` for `https://mcp.example/homebox`), so the gateway
sends `<path>/upload` to that listener unrewritten, with an explicit request
timeout of its own, and everything else through the sidecar. The chart's
HTTPRoute does this, and its NetworkPolicy admits the gateway on the upload
port as well as the proxy port. What a leaked link allows is one attachment on
one entity within ten minutes; it reads nothing and deletes nothing.

### The excluded endpoints are a safety boundary, not an oversight

`/actions/*` (including `wipe-inventory`), authentication and API-key
management, group membership, and bulk import/export have no tool. These are
the calls whose damage a model cannot foresee and cannot undo. `/healthz`
likewise does not call HomeBox: a probe that fails when a dependency blips
restarts or unreadies a pod for something restarting cannot fix. (The sidecar's
own `/readyz`, which waits for the issuer's discovery document, is the pod's
readiness.)

## Transports

`stdio` is the default, so running the binary with no configuration behaves
the way a local MCP client expects. `http` serves the streamable HTTP
transport at `/mcp` on loopback, behind the sidecar; it is what runs in a
cluster, and the chart hardcodes it, because stdio in a pod with no attached
client exits immediately.

The MCP listener binds loopback, not `0.0.0.0`: it has no authentication of its
own. The upload listener and the ops listener bind where the gateway and the
kubelet can reach them, and serve one endpoint each. The MCP SDK answers `403`
to a loopback-bound server when the `Host` header is not loopback; the sidecar
sets `Host` to its upstream (`127.0.0.1:8081`), so point the sidecar at the
loopback address, not a pod IP or a Service name.

### Request size

The sidecar refuses request bodies over `MAX_REQUEST_BYTES` (`413`; its default
is 4 MiB). `homebox_create_attachment` takes `data_base64` for up to 10 MiB of
file, about 13.4 MB on the wire, so the chart sets `proxy.maxRequestBytes` to
16 MiB, with the sidecar's memory limit at 128Mi because it buffers a JSON body
to read the method and tool. The upload link is not behind the sidecar and has
its own limit (`UPLOAD_MAX_BYTES`, up to 1 GiB).

## Startup

The server calls `/users/self` once at startup and logs the authenticated
user, email and group. A bad or expired token fails immediately with a clear
message, rather than on the first tool call as a puzzling per-tool error.
