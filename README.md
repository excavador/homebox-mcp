# homebox-mcp

[Model Context Protocol](https://modelcontextprotocol.io) server for a
[HomeBox](https://homebox.software) inventory, in Go. Ask a model what is in
the attic, add what you just bought, or record that the boiler was serviced.

```
ghcr.io/excavador/homebox-mcp              image, multi-arch amd64 + arm64
oci://ghcr.io/excavador/charts/homebox-mcp chart
```

## Quick start

Mint a HomeBox API key (Settings → API Keys), then point a client at the
binary:

```
claude mcp add homebox \
  -e HOMEBOX_URL=https://homebox.example.com \
  -e HOMEBOX_TOKEN=hb_... \
  -- /path/to/homebox-mcp
```

Full instructions, including the Helm chart, are in
[docs/installation.md](docs/installation.md).

## Documentation

| | |
|---|---|
| [Installation](docs/installation.md) | binary, container and Helm chart; every value and env var |
| [Connecting a client](docs/clients.md) | Claude Code over stdio and HTTP; what to expect from the tools |
| [Architecture](docs/architecture.md) | the request path, why the server authenticates nothing, what is load-bearing |
| [Development](docs/development.md) | devbox, `just check`, testing, releasing, adding a tool |

## What it exposes

The inventory is covered end to end: entities, tags, entity types, templates,
maintenance records and attachments all read, create, update and delete.

| tools | what they cover |
|---|---|
| `homebox_list_entities` `homebox_get_entity` `homebox_create_entity` `homebox_update_entity` `homebox_patch_entity` `homebox_delete_entity` `homebox_duplicate_entity` | entities — **items, not locations** |
| `homebox_entity_tree` `homebox_entity_path` | how entities nest — **this is where locations are** |
| `homebox_list_tags` `homebox_get_tag` `homebox_create_tag` `homebox_update_tag` `homebox_delete_tag` | tags — what older HomeBox called labels |
| `homebox_entity_types` `homebox_create_entity_type` `homebox_update_entity_type` `homebox_delete_entity_type` | the types that distinguish an item from a location |
| `homebox_list_templates` `homebox_get_template` `homebox_create_template` `homebox_update_template` `homebox_delete_template` `homebox_create_item_from_template` | templates |
| `homebox_maintenance` `homebox_create_maintenance` `homebox_update_maintenance` `homebox_delete_maintenance` | maintenance records |
| `homebox_get_attachment` `homebox_update_attachment` `homebox_delete_attachment` | attachments on an entity |
| `homebox_get_asset` `homebox_search_barcode` | look up by printed asset id or barcode |
| `homebox_statistics` `homebox_bill_of_materials` `homebox_currencies` | totals, the CSV report, and the currency list |
| `homebox_custom_fields` `homebox_custom_field_values` | custom field names, and the values in use |
| `homebox_get_configuration` `homebox_set_configuration` | group and user settings, as one document each way |

Every tool carries MCP annotations, so a client can tell a read from a delete
without calling it: reads are `readOnlyHint`, creates are non-destructive, and
updates and deletes are `destructiveHint`.

`/v1/entities` returns items only — locations are reachable through the tree.
The tool descriptions say so, because a model picking a tool reads them: one
that claims to list locations and returns none is worse than no tool at all.

## What it will not do

Some of the API is deliberately unreachable, because these are the calls whose
damage a model cannot see coming and cannot undo:

- **`/actions/*`** — including `ensure-asset-ids`, `zero-item-time-fields` and
  `wipe-inventory`. One of these empties the group.
- **Authentication and API keys** — minting or revoking a credential is not
  inventory work.
- **Group membership and invitations** — who can see the data is not the
  model's call.
- **Bulk import and export** — an import is an undoable mass write.
- **Notifiers** — `set_configuration` will not touch them, so a misread
  instruction cannot redirect or silence alerts.

Configuration is two tools rather than a dozen: one read that returns the group
and user settings together, and one write that takes the same shape back.

## Two things to know before deploying it

**HomeBox accepts exactly one credential: its own API key.** It does not
accept an OIDC token — HomeBox's OIDC support is a browser redirect flow that
ends by *issuing* one of these keys. Data is scoped by **group**, not by
user, so the key sees whatever its owner's group contains.

**The HTTP transport authenticates nothing.** It is built to sit behind a
gateway that validates a token. Exposed directly to a network, it is the
whole inventory, unauthenticated and writable. There is no setting that turns
authentication on, because there is none to turn on — see
[architecture](docs/architecture.md#the-server-authenticates-nothing).

## Why this exists rather than a fork

The obvious starting point, `jeeves5454/Homebox-mcp`, was rejected twice over.

Its **licence is asserted but never granted** — the README and `package.json`
both say MIT, but there is no LICENSE file, GitHub's licence API returns 404,
and no copyright holder is named. The author's intent is clear; intent is not
a grant, and redistribution is the part that needs one.

And it **targets an API that no longer exists**. HomeBox 0.26.2 replaced
`/v1/items`, `/v1/labels` and `/v1/locations` with `/v1/entities` and
`/v1/tags`. Every tool it exposes would 404.

## Licence

MIT, © 2026 Oleg Tsarev.
