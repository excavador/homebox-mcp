# Architecture

A single Go binary that translates MCP tool calls into HomeBox REST calls.
There is no database, no cache and no state: every tool call is one HTTP
request to HomeBox, and the process can be restarted or scaled at will.

## The request path

    Claude Code (or any MCP client)
      │  presents client_id = an HTTPS URL to its own Client ID Metadata
      │  Document (CIMD) -- no pre-registration, no DCR
      │  gets back an access token: iss=access-roster, aud=this resource
      │  MCP over streamable HTTP, Authorization: Bearer <access token>
      ▼
    homebox-mcp  (TRANSPORT=http, listening on :8080/mcp)
      │  validates the token itself: signature against access-roster's
      │  JWKS, iss, and aud == RESOURCE_URL (RFC 8707) -- see auth.go
      │  Authorization: Bearer <HomeBox API key>
      ▼
    HomeBox  /api/v1/...

Two different credentials, and conflating them is the mistake this design
exists to prevent:

- The **caller's** token is an access-roster access token identifying a
  person, bound to this server's own resource URL as its audience.
  `homebox-mcp` verifies it itself, in-process — see
  [design/cimd-auth.md](design/cimd-auth.md) for why authentication moved
  from a perimeter gateway into the binary.
- The **server's** token is a HomeBox API key, held in a Kubernetes Secret and
  read once at startup. HomeBox accepts nothing else — its OIDC support is a
  browser redirect flow that ends by *issuing* one of these keys.

### The server validates its own bearer tokens

`ISSUER_URL` and `RESOURCE_URL` are both required for the http transport --
there is no gateway fallback. The server verifies a request's bearer token
against access-roster's own JWKS, requires `aud` to equal `RESOURCE_URL`
exactly (RFC 8707), and serves its own OAuth 2.0 Protected Resource Metadata
at `/.well-known/oauth-protected-resource` (RFC 9728) so a compliant client
can discover access-roster without being told out of band. See
[design/cimd-auth.md](design/cimd-auth.md).

Exposed with no `ISSUER_URL`/`RESOURCE_URL` set, the process refuses to
start the http transport at all -- there is no way to accidentally run it
unauthenticated.

### One shared identity, and what that costs

Every caller access-roster admits is still one HomeBox API key as far as
HomeBox itself is concerned: the server makes every request with it, so
HomeBox attributes every change to that key rather than to the person who
asked for it. **HomeBox's audit trail cannot tell one user's deletion from
another's** -- access-roster identifies *who may reach this server*, not who
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

### The excluded endpoints are a safety boundary, not an oversight

`/actions/*` (including `wipe-inventory`), authentication and API-key
management, group membership, and bulk import/export have no tool. These are
the calls whose damage a model cannot foresee and cannot undo. `/healthz`
likewise does not call HomeBox: a readiness probe that fails when a
dependency blips takes a pod out of service for something restarting cannot
fix.

## Transports

`stdio` is the default, so running the binary with no configuration behaves
the way a local MCP client expects. `http` serves the streamable HTTP
transport at `/mcp`, which is what runs in a cluster; the chart hardcodes it,
because stdio in a pod with no attached client exits immediately.

The HTTP listener binds `0.0.0.0`, not loopback — in a container, binding to
`127.0.0.1` means nothing can reach it, including the kubelet's probes.

## Startup

The server calls `/users/self` once at startup and logs the authenticated
user, email and group. A bad or expired token fails immediately with a clear
message, rather than on the first tool call as a puzzling per-tool error.
