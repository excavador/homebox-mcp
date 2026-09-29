# Connecting a client

## Claude Code, locally over stdio

The simplest setup: the binary runs on your machine, as a subprocess of the
client, and nothing is exposed to a network.

    claude mcp add homebox \
      -e HOMEBOX_URL=https://homebox.example.com \
      -e HOMEBOX_TOKEN=hb_... \
      -- /path/to/homebox-mcp

Then `/mcp` in Claude Code should list `homebox` as connected. Ask it
something that needs a read — "what tags exist in my inventory?" — to confirm
it reaches HomeBox rather than merely starting.

Scope it with `-s user` to make it available in every project rather than
just the current directory.

## Claude Code, over HTTP

    claude mcp add --transport http homebox https://mcp.example.com/homebox

Claude Code follows the MCP authorization spec: it requests the endpoint,
gets a `401` naming this server's own protected-resource metadata, follows
that to discover access-roster as the authorization server, and opens a
browser for you to log in. It identifies itself with a Client ID Metadata
Document it already serves at a fixed URL of its own
(`https://claude.ai/oauth/claude-code-client-metadata`) — nothing to
register, nothing to configure on the client side, provided access-roster's
policy allow-lists `claude.ai` under `client_documents.origins` (see
[design/cimd-auth.md](design/cimd-auth.md)).

If the deployment predates that policy change, or the client you are using
does not support CIMD, connect with a manually-minted token instead:

    claude mcp add --transport http homebox https://mcp.example.com/homebox \
      --header "Authorization: Bearer ..."

## Other MCP clients

Anything implementing MCP 2025-06-18 works. The two transports are the
standard ones:

- **stdio** — the client spawns the binary and talks over its stdin/stdout.
  Configure it as a command with `HOMEBOX_URL` and `HOMEBOX_TOKEN` in the
  environment.
- **streamable HTTP** — `POST` to `/mcp`. The server issues an
  `Mcp-Session-Id` on initialize which later requests must carry.

## What to expect from the tools

A few things about HomeBox's own model that will otherwise look like bugs:

**Locations are entities.** `homebox_list_entities` returns items only. The
locations are in `homebox_entity_tree`, which is how entities nest. A model
asking to "list locations" and reaching for `list_entities` will get an empty
answer that looks like an empty inventory; the tool descriptions say this,
because that is where a model reads.

**Tags are what older HomeBox called labels.** Anything written against the
pre-0.26 API — including most of what a model has seen about HomeBox — refers
to `/items`, `/labels` and `/locations`, none of which exist any more.

**`homebox_update_entity` replaces the object**: fields omitted from the body
are cleared. Reach for `homebox_patch_entity` to change a few fields and leave
the rest alone -- it reads, merges and writes for you.

Neither will move an entity by accident. HomeBox does not report an entity's
parent when you read it, so the obvious read-modify-write would put everything
at the top level; both tools carry the parent forward unless you pass
`parentId` yourself.

**Everything is group-scoped.** The server sees whatever its API key's group
contains, and every change it makes is attributed to that key rather than to
the person who asked — see
[architecture](architecture.md#one-shared-identity-and-what-that-costs).

## Reading the annotations

Every tool declares whether it reads or writes, so a client can decide what
to confirm before calling:

| annotation | tools | meaning |
|---|---|---|
| `readOnlyHint` | 19 | cannot change anything |
| `destructiveHint: false` | 7 | creates something new |
| `destructiveHint: true` | 14 | updates or deletes |

If your client offers per-tool approval, the destructive set is the one worth
gating. The tools that would be genuinely unrecoverable — wiping the
inventory, revoking credentials, bulk import — are not exposed at all, so no
amount of client misconfiguration reaches them.
