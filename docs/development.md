# Development

## Setup

The toolchain is pinned with [devbox](https://www.jetify.com/devbox):

    devbox shell        # or: direnv allow, if you use direnv

That provides Go, just, helm and goreleaser at the versions CI uses. Without
devbox you need those four yourself.

`CGO_ENABLED=0` is set in `devbox.json`: the binary is static, so no C
toolchain is needed and the image can be distroless.

## The one command

    just check

That is what CI runs — vet, test, build, plus the chart checks. If it passes
locally it passes in CI, and the reverse.

It builds `./cmd/homebox-mcp` explicitly rather than relying on `go build
./...`, because a repo whose main package has gone missing still passes the
latter — the library packages compile on their own. That is exactly how
`cmd/` got left out of the first release: `.gitignore` swallowed it and every
check stayed green.

## Running it against a real HomeBox

    just run https://homebox.example.com hb_...              # stdio
    just serve https://homebox.example.com hb_... 127.0.0.1:8080   # http

`serve` binds loopback by default here, unlike the production default — this
server authenticates nothing, so a development instance should not be on a
network interface.

## Testing

Tests drive the **registered tools** over an in-memory MCP transport against
a stub HomeBox, rather than calling helper functions directly. That is
deliberate: every bug this project has shipped was in how a tool wired itself
to the client, not in the client, and a test that calls `call()` directly
proves only that `call()` works.

    func TestSomething(t *testing.T) {
        cs := connect(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // stand in for HomeBox
        }))
        res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{...})
    }

### Confirm a test fails before believing it

Every regression test here was checked by putting the bug back and watching
it fail. This is not ceremony — it has caught two things:

- `TestEveryToolIsAnnotated` once asserted only that `New()` returned
  non-nil. It passed forever while checking nothing the name promised.
- A later mutation of that test *appeared* to show the assertion was weak.
  It was not: the mutation had not applied, because the replacement string
  did not match gofmt's alignment. A mutation test is evidence only if you
  confirm the mutation landed.

## The chart

`just chart` lints and renders it, and then checks the things that should
**fail**:

- rendering with no `homebox.url`
- a URL ending in `/api` (the server appends it, so this would double)
- an unknown values key such as `replicaCounts`

A chart exercised only with correct values has not been tested. The schema
and the guards exist to reject input, so the checks that matter are the ones
expecting a refusal.

`values.schema.json` sets `additionalProperties: false`, which is what turns
a typo into an error instead of a silently ignored setting.

## Releasing

Tag it:

    git tag -a v0.4.0 -F <message-file>
    git push origin v0.4.0

The `Release` workflow (`truvity/ci-workflows`) then builds and publishes
both artifacts:

- the **image** via ko — no Dockerfile, multi-arch `amd64` + `arm64`, which
  is not optional on arm boards — to `ghcr.io/excavador/homebox-mcp`
- the **chart** via helmctl to `oci://ghcr.io/excavador/charts/homebox-mcp`

Bump `charts/homebox-mcp/Chart.yaml` (`version` and `appVersion`) in the same
commit as the code, so the chart's default image tag matches what was built.

Preview what a release would produce without publishing:

    just snapshot

Note goreleaser refuses to run on a dirty tree, and an uncommitted
`devbox.lock` counts.

**Write commit and tag messages to a file and use `-F`.** Backticks inside a
double-quoted shell string execute; a message containing `` `foo` `` will
silently lose words or run commands.

## Adding a tool

1. Register it in the right `add*` function in `internal/server/server.go`.
2. Give it annotations — `readOnly()`, `creates()`, `updates()` or
   `deletes()`. A tool with none fails `TestEveryToolIsAnnotated`.
3. Write the description for a model, not a developer. It is the only thing
   a model reads when choosing between tools, so it should say what the tool
   returns and, where HomeBox is surprising, what it does *not* (see
   `list_entities`, which does not return locations).
4. Check the response shape. If the endpoint answers with a top-level array,
   `call()` already wraps it as `{"items": ...}`; if it answers with
   something that is not JSON at all, it needs `Text()` instead — the
   bill-of-materials report is the example.
5. Test it against a stub, then against a real HomeBox. Three of this
   project's bugs were invisible to unit tests and obvious on first contact
   with a live server.
