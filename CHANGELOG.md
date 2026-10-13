# Changelog

Release notes for each tag are also generated from the commit messages and
published on GitHub; this file is the hand-written summary.

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
