# Design: CIMD authentication

This server used to authenticate nothing and rely entirely on a gateway
in front of it. It now validates a bearer token itself, against
[access-roster](https://github.com/truvity/access-roster) (v1.29.0+,
which introduced both the `resources` table and `client_documents`; the
estate runs v1.39.0+), following the Model Context Protocol's
authorization spec. This document records what changed and why, so the
next person touching auth here does not have to reconstruct the reasoning
from a diff.

## What CIMD is, and why it fits an MCP server

A **Client ID Metadata Document (CIMD)** is the MCP authorization spec's
recommended way for a client to identify itself: instead of registering
with the authorization server ahead of time (a row in a policy) or
dynamically at connect time (RFC 7591, which the spec deprecated), the
client's `client_id` is simply an HTTPS URL. That URL serves a small JSON
document describing the client (`client_id` matching the URL itself,
`client_name`, `redirect_uris`, ...). The authorization server fetches it,
validates it, and treats the client as an ordinary public client for that
request — nothing is registered, nothing accumulates.

This fits an MCP server precisely because the population of clients is
not something the server operator controls or can enumerate: an editor,
a hosted assistant, whatever a person runs on their own laptop. Access
control here was never about *which piece of software* is asking — it is
about *which person* is asking, which is what access-roster's own
`requires` groups already decide. CIMD removes the bureaucracy of
pre-registering a client id for every piece of software without granting
that software anything a declared client would not also have received.

Confirmed empirically before this design was finalized: this session's
own Claude Code client, pointed at an authorization server that advertises
`client_id_metadata_document_supported: true` and no `registration_endpoint`,
presented `client_id=https://claude.ai/oauth/claude-code-client-metadata`
on `/authorize` — a real, well-formed CIMD document (`client_id` matching
the URL, `redirect_uris: [http://localhost/callback, http://127.0.0.1/callback]`,
`token_endpoint_auth_method: none`). `claude.ai` is therefore the origin
to allow-list for that client.

## What changed here

**Removed:**

- Nothing was removed from this repo's own auth, because it never had
  any — the whole point of this change is that "no auth, gateway does it"
  is no longer the story.

**Added:**

- Auth from access-roster's shared Go library,
  `github.com/truvity/access-roster/identity/resource`: it wraps the MCP
  handler with a bearer-token check against access-roster and serves this
  server's own RFC 9728 Protected Resource Metadata at the well-known path
  for the resource URL. (This was first a local `internal/server/auth.go`;
  that copy was identical in every MCP server and now lives in the library.)
- Two new required settings for the http transport: `ISSUER_URL`
  (access-roster's own URL) and `RESOURCE_URL` (this server's own
  external URL — the RFC 8707 resource indicator / JWT audience). Both
  plain strings: neither is a secret, and a CIMD client never has one
  either.
- `WWW-Authenticate: Bearer resource_metadata="..."` on an unauthenticated
  401, per RFC 9728 §5.1, so a compliant client discovers the issuer
  without being told out of band.

**What did NOT change:**

- The upstream HomeBox credential (`HOMEBOX_TOKEN`) and everything about
  how this server talks to HomeBox.
- No group/scope vocabulary was added here. `resource.Resource.Protect` requires only
  that access-roster vouches for the caller at all — *who* may reach this
  resource is entirely access-roster's `resources.<this URL>.requires`,
  read once, in one place, not duplicated into a second check here.

## The exact access-roster policy shape this expects

```yaml
resources:
  https://mcp.excavador.xyz/homebox:
    requires: [all:homebox-mcp:user]
    ttl_cap: 5m
    display_name: HomeBox MCP

client_documents:
  origins:  [claude.ai]
  requires: [all:homebox-mcp:user]   # or a broader "may use any CIMD client" group
  ttl_cap:  10m
```

`resources.<uri>.requires` and `client_documents.requires` are two
different gates that both apply: the first says who may reach *this*
server, the second says who may use *any* client identified by a document
served from an allow-listed origin. See access-roster's own
[docs/connect/mcp.md](https://github.com/truvity/access-roster/blob/master/docs/connect/mcp.md)
for the full mechanism.

This is a deployment's (`opwerm/nexus`, for hive) concern, not this
repo's — it is documented here so the shape stays associated with the
code it configures.

## The scope access-roster insists on

access-roster's `/authorize` refuses a request that carries no `scope` at
all ("The scope of your request is missing"). Claude Code's CIMD client
document names none, and per the MCP authorization spec's scope-selection
order a client tries `WWW-Authenticate`'s `scope` first, then the PRM's
`scopes_supported`, and otherwise sends none — so a scope-less client had
no way to reach an issuer that requires one.

The resource now advertises a `scope` (default `openid`, which access-roster
accepts) in both places:

- the PRM's `scopes_supported`, and
- the 401 challenge, next to `resource_metadata`:
  `Bearer resource_metadata="...", scope="openid"`.

It is `Scope` in the library's `resource.Config` and a `--scope` /
`SCOPE` flag (`cmd/homebox-mcp/main.go`), mirroring how `ISSUER_URL` and
`RESOURCE_URL` are wired, and a chart value, `auth.scope` (also default
`openid`) — so an estate that needs a richer scope than `openid` sets it
without a new release. Kept estate-neutral on purpose: this repo is
public and carries no estate's own policy.

The scope is **advertised only, never enforced here** — same as before:
*who* may reach this resource is still entirely access-roster's
`resources.<uri>.requires`, read once. An empty scope omits both fields
rather than advertising an empty list, which is not equivalent to
omitting the field.

## What a client actually needs to do

Nothing beyond following the MCP authorization spec: discover this
server's protected-resource metadata (via `WWW-Authenticate` or the
well-known path), discover access-roster as the authorization server,
and either present a pre-registered client id (if the deployment declared
one) or a CIMD `client_id` URL if it has one and the issuer advertises
`client_id_metadata_document_supported: true`. A client with neither has
no way to reach this server — the mechanism does not fall back to
unauthenticated access.

## Behaviour differences after moving to the shared library

Moving to `identity/resource` kept the contract above (bearer check against
`ISSUER_URL`/`RESOURCE_URL`, the PRM, the 401 challenge with
`resource_metadata` and `scope`, the `--scope`/`SCOPE` setting) and changed
two things:

- **Well-known path.** The metadata is served at the RFC 9728 §3.1 path of
  the resource URL: the well-known prefix followed by the resource's own
  path (`/.well-known/oauth-protected-resource/netbox` for
  `https://mcp.example.com/netbox`). A resource at the root of its host
  (`https://mcp.example.com/`, or no path) is served at the bare prefix. The
  bare prefix is also still answered for a resource with a path, as an alias
  for a gateway that rewrites to it; with several resources on one host,
  route the bare form to one pod only.
- **Issuer unreachable answers 503.** If the issuer's discovery document or
  keys cannot be fetched, a request now gets `503 Service Unavailable`
  rather than a `401`, so a legitimate caller is not sent to sign in again
  during an outage. A token that was presented and does not verify gets
  `401` with `error="invalid_token"` added to the challenge.
