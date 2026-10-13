# Everything goes through here; CI runs the same recipes.

# Build and vet.
#
# Builds the COMMAND explicitly, not just ./... — a repo whose main package
# is missing still passes `go build ./...`, because the library packages
# compile on their own. That is exactly how cmd/ got left out of the first
# release: .gitignore swallowed it and every check stayed green.
check:
    go vet ./...
    go test ./...
    go build ./...
    go build -o /dev/null ./cmd/homebox-mcp
    just chart

# Lint and render the chart, and prove its guards still refuse bad input.
#
# A chart exercised only with correct values has not been tested: the
# schema and the required-value guards exist to REJECT things, so the
# checks that matter are the ones expecting failure.
auth_values := "--set auth.issuerUrl=https://access.example --set auth.resourceUrl=https://mcp.example/homebox"

chart:
    helm lint charts/homebox-mcp \
        --set homebox.url=http://homebox --set homebox.existingSecret=x {{auth_values}}
    helm template t charts/homebox-mcp \
        --set homebox.url=http://homebox --set homebox.existingSecret=x {{auth_values}} > /dev/null
    @# missing required values
    @! helm template t charts/homebox-mcp >/dev/null 2>&1 \
        || (echo "FAIL: rendered without homebox.url"; exit 1)
    @# url must not carry /api -- the server appends it
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox/api \
        --set homebox.existingSecret=x {{auth_values}} >/dev/null 2>&1 \
        || (echo "FAIL: accepted a url ending in /api"; exit 1)
    @# unknown keys are typos, not options
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set replicaCounts=2 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an unknown values key"; exit 1)
    @# no gateway fallback any more -- auth.issuerUrl/resourceUrl are required
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.issuerUrl / auth.resourceUrl"; exit 1)
    @# reloader reads its annotation on the Deployment, not the pod template.
    @# An annotation that lands in the wrong place looks right and never fires.
    @helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} \
        --set-string 'deploymentAnnotations.reloader\.stakater\.com/auto=true' \
      | awk '/^kind: Deployment/,/^spec:/' | grep -q 'reloader.stakater.com/auto' \
      || (echo "FAIL: deploymentAnnotations did not reach the Deployment"; exit 1)
    @# auth.scope defaults to openid and reaches SCOPE, so an estate that
    @# never sets it still advertises a scope access-roster accepts.
    @helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} \
      | grep -A1 'name: SCOPE' | grep -q '"openid"' \
      || (echo "FAIL: auth.scope did not default to openid on SCOPE"; exit 1)
    @# and an estate needing a different scope can still set it.
    @helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set auth.scope=openid+mcp \
      | grep -A1 'name: SCOPE' | grep -q '"openid+mcp"' \
      || (echo "FAIL: auth.scope did not override SCOPE"; exit 1)
    @# upload settings reach the container, and a large byte count must not
    @# render as 1.048576e+08 -- Helm reads numbers from a values file as floats.
    @helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} \
        --set upload.maxBytes=104857600 --set upload.ttl=20m --set upload.timeout=1h --set homebox.timeout=45s \
      | tr -d '\n' | grep -q 'UPLOAD_MAX_BYTES *value: "104857600"' \
      || (echo "FAIL: upload.maxBytes did not reach UPLOAD_MAX_BYTES as an integer"; exit 1)
    @helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set upload.ttl=20m --set upload.timeout=1h --set homebox.timeout=45s \
      | tr -d '\n' | grep -q 'UPLOAD_TTL *value: "20m".*UPLOAD_TIMEOUT *value: "1h"' \
      || (echo "FAIL: upload.ttl / upload.timeout did not reach the container"; exit 1)
    @# nonsense settings are refused at render time, not at pod start
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set upload.maxBytes=0 >/dev/null 2>&1 \
        || (echo "FAIL: accepted upload.maxBytes=0"; exit 1)
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set upload.timeout=soon >/dev/null 2>&1 \
        || (echo "FAIL: accepted upload.timeout=soon"; exit 1)
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{auth_values}} --set upload.maxBytes=5368709120 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an upload.maxBytes over 1 GiB"; exit 1)
    @echo "chart ok: renders, refuses missing values (including auth), a /api url and unknown keys, annotates the Deployment, wires auth.scope, and passes and checks the upload settings"

# Run against a HomeBox instance over stdio (the default transport).
run url token:
    HOMEBOX_URL={{url}} HOMEBOX_TOKEN={{token}} go run ./cmd/homebox-mcp

# Serve over HTTP, as it runs in a cluster. Requires an access-roster
# issuer and this server's own external resource URL -- see
# docs/design/cimd-auth.md.
serve url token issuer resource addr="127.0.0.1:8080":
    HOMEBOX_URL={{url}} HOMEBOX_TOKEN={{token}} TRANSPORT=http ADDR={{addr}} \
        ISSUER_URL={{issuer}} RESOURCE_URL={{resource}} \
        go run ./cmd/homebox-mcp

# What the release will build, without publishing.
#
# KO_DOCKER_REPO is required even when publishing is skipped -- ko needs a
# repository to name the image it builds, and fails before building without
# one. CI passes the real registry; locally any name will do, and ko.local
# keeps the result out of a registry namespace that means something.
snapshot:
    KO_DOCKER_REPO=ko.local goreleaser release --snapshot --clean --skip=publish
