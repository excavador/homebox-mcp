# Changelog

Release notes for each tag are also generated from the commit messages and
published on GitHub; this file is the hand-written summary.

## 0.9.0 - unreleased

The server and its chart now run behind the sluis `resource-proxy` sidecar. The
token check, the protected-resource metadata and the 401 challenge move out of
the binary into the sidecar; the signed upload link gets a listener of its own.
**This release is breaking**: read "Breaking changes" before upgrading.

### Changed

- The HTTP transport has no authentication code and no longer imports
  `github.com/truvity/sluis`. The sidecar verifies every caller's access token,
  serves the RFC 9728 metadata, writes an `mcp_request` audit line and forwards
  to the server on `127.0.0.1:8081/mcp`.
- The MCP endpoint listens on `127.0.0.1:8081` and the server refuses to start
  on any non-loopback `--addr`, unless `--allow-non-loopback`
  (`ALLOW_NON_LOOPBACK=true`) says so. Fail closed: a wrong address can no
  longer publish the inventory.
- The signed upload link moved to its own listener, `--upload-addr`
  (`UPLOAD_ADDR`; 8082 on the pod IP in the chart), which serves that endpoint
  and nothing else and is NOT behind the sidecar. It serves
  `PUT <path of PUBLIC_URL>/upload/{token}`, the path in the link a client is
  given (previously `PUT /mcp/upload/{token}` behind a gateway rewrite).
- `/healthz` moved to its own listener, `--ops-addr` (`OPS_ADDR`, `:9090`).
- Chart: two containers (server and `proxy`), Service ports `http` (the proxy)
  and `upload`, a NetworkPolicy and an optional HTTPRoute shipped by the chart,
  Pod Security `restricted`, no ServiceAccount token, readiness on the proxy's
  `/readyz`. The proxy's `MAX_REQUEST_BYTES` is 16 MiB (`proxy.maxRequestBytes`)
  and its memory limit 128Mi, because `data_base64` attachments reach about
  13.4 MB on the wire.

### Breaking changes

- Removed flags and environment variables `--issuer-url` / `ISSUER_URL`,
  `--resource-url` / `RESOURCE_URL` and `--scope` / `SCOPE` from the server.
  They are the sidecar's now; set on the server they make it exit. The server's
  external URL is `--public-url` / `PUBLIC_URL` (the base of upload links).
- `--addr` / `ADDR` defaults to `127.0.0.1:8081` (was `0.0.0.0:8080`) and
  refuses a non-loopback address.
- The server answers no `/healthz`, no `/.well-known/oauth-protected-resource`
  and no `/mcp/upload/{token}` on its MCP port.
- Chart: `proxy.image.tag` and `proxy.image.digest` are required (no default
  sidecar version), and the chart does not render without `networkPolicy.ingressFrom`
  (or `networkPolicy.enabled: false`). `auth.issuerUrl`, `auth.resourceUrl` and
  `auth.scope` keep their names and now configure the sidecar; `auth.resourceUrl`
  must be https, have a path and no trailing slash. `service.port` is the
  Service port of the proxy; it is no longer the container's listen port.
- The routing in front of the chart changes together with it: the gateway must
  send the resource path to the proxy port WITHOUT rewriting it to `/mcp` (the
  proxy maps it), and `<path>/upload` to the upload port (also without a
  rewrite). `httpRoute.enabled` renders exactly that. A cutover with the old
  rewriting route in place answers 404 until the route moves with the chart.
  The upload link itself, `<resource URL>/upload/<token>`, is unchanged.

### Added

- Chart: `proxy.*`, `service.uploadPort`, `networkPolicy.*`, `httpRoute.*`.

## 0.8.0 - 2026-10-13

### Added

- `homebox_prepare_attachment_upload`: a one-time upload link for a file on the
  caller's own machine. The caller sends it with
  `curl -X PUT --data-binary @file <link>`; the bytes never pass through the
  model. The link is signed, stateless, bound to one entity and one attachment
  type, and works once (a refused file does not use it up). The file is
  streamed on to HomeBox, so memory stays flat whatever its size. Served at
  `PUT /mcp/upload/{token}` without a bearer token, because a shell cannot
  present one; see `docs/architecture.md`.
- Settings, each as flag, environment variable and chart value, validated at
  startup and at render time:
  `UPLOAD_MAX_BYTES` (`upload.maxBytes`, default 10 MiB, up to 1 GiB),
  `UPLOAD_TTL` (`upload.ttl`, default 10m), `UPLOAD_TIMEOUT`
  (`upload.timeout`, default 15m) and `HOMEBOX_TIMEOUT` (`homebox.timeout`,
  default 30s).

### Changed

- The request timeout towards HomeBox, previously fixed at 30 seconds, is now
  `HOMEBOX_TIMEOUT`. A streamed upload is not bound by it.

### Operations

- Raising `UPLOAD_MAX_BYTES` above 10 MiB needs HomeBox's own
  `HBOX_WEB_MAX_UPLOAD_SIZE` (in MiB) raised with it, or HomeBox refuses the
  file instead.

## 0.7.0

### Added

- `homebox_create_attachment`: upload a photo or file to an entity from a
  public https URL (public addresses only, size and type checked) or as
  base64, up to 10 MiB.

## 0.6.1 and earlier

Entities, tags, entity types, templates, maintenance records and
attachments, read and written; access-roster authentication validated by the
server itself.
