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
proxy_values := "--set proxy.image.tag=1.84.0 --set proxy.image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000"
gateway_values := "--set-json 'networkPolicy.ingressFrom=[{\"podSelector\":{\"matchLabels\":{\"app\":\"gateway\"}}}]'"
base_values := "--set homebox.url=http://homebox --set homebox.existingSecret=x " + auth_values + " " + proxy_values + " " + gateway_values
route_values := "--set httpRoute.enabled=true --set-json 'httpRoute.parentRefs=[{\"name\":\"mcp\"}]'"

chart:
    helm lint charts/homebox-mcp {{base_values}}
    helm template t charts/homebox-mcp {{base_values}} > /dev/null
    helm template t charts/homebox-mcp {{base_values}} {{route_values}} > /dev/null
    @# missing required values
    @! helm template t charts/homebox-mcp >/dev/null 2>&1 \
        || (echo "FAIL: rendered without homebox.url"; exit 1)
    @# url must not carry /api -- the server appends it
    @! helm template t charts/homebox-mcp {{base_values}} --set homebox.url=http://homebox/api >/dev/null 2>&1 \
        || (echo "FAIL: accepted a url ending in /api"; exit 1)
    @# unknown keys are typos, not options
    @! helm template t charts/homebox-mcp {{base_values}} --set replicaCounts=2 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an unknown values key"; exit 1)
    @# the sidecar is what authenticates: no issuer, no resource URL, no render
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox \
        --set homebox.existingSecret=x {{proxy_values}} {{gateway_values}} >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.issuerUrl / auth.resourceUrl"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set auth.issuerUrl=http://access.example >/dev/null 2>&1 \
        || (echo "FAIL: accepted a plain-http issuer"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set auth.resourceUrl=http://mcp.example/homebox >/dev/null 2>&1 \
        || (echo "FAIL: accepted a plain-http resource URL"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set auth.resourceUrl=https://mcp.example/homebox/ >/dev/null 2>&1 \
        || (echo "FAIL: accepted a trailing slash on auth.resourceUrl (aud is compared byte for byte)"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set auth.resourceUrl=https://mcp.example >/dev/null 2>&1 \
        || (echo "FAIL: accepted a resource URL with no path"; exit 1)
    @# the sidecar has no default version: tag AND digest are required
    @! helm template t charts/homebox-mcp {{base_values}} --set proxy.image.tag= >/dev/null 2>&1 \
        || (echo "FAIL: rendered without proxy.image.tag"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set proxy.image.digest= >/dev/null 2>&1 \
        || (echo "FAIL: rendered without proxy.image.digest"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set proxy.image.digest=sha256:abc >/dev/null 2>&1 \
        || (echo "FAIL: accepted a malformed proxy.image.digest"; exit 1)
    @# a NetworkPolicy that admits nobody is refused, not rendered
    @! helm template t charts/homebox-mcp --set homebox.url=http://homebox --set homebox.existingSecret=x \
        {{auth_values}} {{proxy_values}} >/dev/null 2>&1 \
        || (echo "FAIL: rendered an enabled NetworkPolicy with no ingressFrom"; exit 1)
    @# a route with no Gateway to attach to is refused
    @! helm template t charts/homebox-mcp {{base_values}} --set httpRoute.enabled=true >/dev/null 2>&1 \
        || (echo "FAIL: rendered an HTTPRoute with no parentRefs"; exit 1)
    @# reloader reads its annotation on the Deployment, not the pod template.
    @# An annotation that lands in the wrong place looks right and never fires.
    @helm template t charts/homebox-mcp {{base_values}} \
        --set-string 'deploymentAnnotations.reloader\.stakater\.com/auto=true' \
      | awk '/^kind: Deployment/,/^spec:/' | grep -q 'reloader.stakater.com/auto' \
      || (echo "FAIL: deploymentAnnotations did not reach the Deployment"; exit 1)
    @# the sidecar gets the issuer, the resource URL, the scope and the body cap,
    @# and the server container gets NONE of the auth settings.
    @just _chart-sidecar
    @# upload settings reach the container, and a large byte count must not
    @# render as 1.048576e+08 -- Helm reads numbers from a values file as floats.
    @helm template t charts/homebox-mcp {{base_values}} \
        --set upload.maxBytes=104857600 --set upload.ttl=20m --set upload.timeout=1h --set homebox.timeout=45s \
      | tr -d '\n' | grep -q 'UPLOAD_MAX_BYTES *value: "104857600"' \
      || (echo "FAIL: upload.maxBytes did not reach UPLOAD_MAX_BYTES as an integer"; exit 1)
    @helm template t charts/homebox-mcp {{base_values}} --set upload.ttl=20m --set upload.timeout=1h --set homebox.timeout=45s \
      | tr -d '\n' | grep -q 'UPLOAD_TTL *value: "20m".*UPLOAD_TIMEOUT *value: "1h"' \
      || (echo "FAIL: upload.ttl / upload.timeout did not reach the container"; exit 1)
    @# nonsense settings are refused at render time, not at pod start
    @! helm template t charts/homebox-mcp {{base_values}} --set upload.maxBytes=0 >/dev/null 2>&1 \
        || (echo "FAIL: accepted upload.maxBytes=0"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set upload.timeout=soon >/dev/null 2>&1 \
        || (echo "FAIL: accepted upload.timeout=soon"; exit 1)
    @! helm template t charts/homebox-mcp {{base_values}} --set upload.maxBytes=5368709120 >/dev/null 2>&1 \
        || (echo "FAIL: accepted an upload.maxBytes over 1 GiB"; exit 1)
    @echo "chart ok: renders, refuses missing or malformed auth and sidecar pin, a /api url, unknown keys and a policy that admits nobody; sidecar wired; route splits the upload link from the proxy; upload settings checked"

# What the rendered chart must look like around the sidecar and the listeners.
# Kept apart so each assertion can name what it protects.
_chart-sidecar:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(helm template t charts/homebox-mcp {{base_values}} {{route_values}} --set upload.timeout=15m)
    fail() { echo "FAIL: $1"; exit 1; }
    has() { grep -qE -- "$1" <<<"$out" || fail "$2"; }
    lacks() { ! grep -qE -- "$1" <<<"$out" || fail "$2"; }

    # --- the proxy container
    has 'image: "ghcr.io/truvity/sluis/resource-proxy:1.84.0@sha256:0{64}"' "proxy not pinned by tag and digest"
    has 'name: MAX_REQUEST_BYTES' "proxy has no MAX_REQUEST_BYTES"
    tr -d '\n' <<<"$out" | grep -q 'MAX_REQUEST_BYTES *value: "16777216"' || fail "MAX_REQUEST_BYTES is not 16 MiB as an integer"
    tr -d '\n' <<<"$out" | grep -q 'name: RESOURCE_URL *value: "https://mcp.example/homebox"' || fail "RESOURCE_URL is not auth.resourceUrl byte for byte"
    tr -d '\n' <<<"$out" | grep -q 'name: ISSUER_URL *value: "https://access.example"' || fail "ISSUER_URL not wired"
    tr -d '\n' <<<"$out" | grep -q 'name: SCOPE *value: "openid"' || fail "SCOPE did not default to openid"
    tr -d '\n' <<<"$out" | grep -q 'name: UPSTREAM *value: "http://127.0.0.1:8081/mcp"' || fail "UPSTREAM is not the loopback MCP endpoint"
    has 'path: /readyz' "proxy readiness is not /readyz"

    # --- the server container: loopback MCP, no auth settings, no MCP port
    tr -d '\n' <<<"$out" | grep -q 'name: ADDR *value: "127.0.0.1:8081"' || fail "server MCP address is not loopback"
    tr -d '\n' <<<"$out" | grep -qE 'name: UPLOAD_ADDR *value: "\$\(POD_IP\):8082"' || fail "upload listener is not on the pod IP"
    has 'fieldPath: status.podIP' "POD_IP is not the downward API pod IP"
    tr -d '\n' <<<"$out" | grep -q 'name: PUBLIC_URL *value: "https://mcp.example/homebox"' || fail "PUBLIC_URL not wired"
    lacks 'containerPort: 8081' "the MCP port is declared as a container port"
    has 'automountServiceAccountToken: false' "the ServiceAccount token is mounted"

    # --- the Service exposes the proxy port and the upload port only
    svc=$(awk '/^kind: Service/,/^---/' <<<"$out")
    [ "$(grep -c 'targetPort:' <<<"$svc")" = 2 ] || fail "the Service does not expose exactly two ports"
    grep -q 'targetPort: http' <<<"$svc" && grep -q 'targetPort: upload' <<<"$svc" || fail "the Service ports are not http and upload"
    ! grep -q 'targetPort: ops' <<<"$svc" || fail "the Service exposes the ops port"

    # --- the route: upload straight to its port with a timeout, MCP through the proxy with none
    route=$(awk '/^kind: HTTPRoute/,/^---/' <<<"$out")
    up=$(awk '/value: \/homebox\/upload/{f=1} f{print} /weight:/{if(f) exit}' <<<"$route")
    grep -q 'port: 8082' <<<"$up" || fail "the upload route does not go to the upload port"
    grep -q 'request: 15m' <<<"$up" || fail "the upload route has no explicit timeout"
    ! grep -q 'URLRewrite' <<<"$up" || fail "the upload route rewrites the link"
    [ "$(grep -c 'request:' <<<"$route")" = 1 ] || fail "a rule other than the upload one has a request timeout"
    [ "$(grep -c 'port: 8082' <<<"$route")" = 1 ] || fail "more than the upload rule reaches the upload port"

    # --- the policy: gateway to the proxy and upload ports, never to ops
    np=$(awk '/^kind: NetworkPolicy/,/^---/' <<<"$out")
    ing=$(awk '/^  ingress:/,/^  egress:/' <<<"$np")
    grep -q 'port: 8080' <<<"$ing" && grep -q 'port: 8082' <<<"$ing" || fail "the policy does not admit the proxy and upload ports"
    ! grep -q 'port: 9090' <<<"$ing" || fail "the policy admits the ops port"
    ! grep -q 'port: 8081' <<<"$ing" || fail "the policy admits the MCP port"
    echo "sidecar render ok"

# Run against a HomeBox instance over stdio (the default transport).
run url token:
    HOMEBOX_URL={{url}} HOMEBOX_TOKEN={{token}} go run ./cmd/homebox-mcp

# Serve over HTTP, as it runs behind the resource-proxy sidecar. The MCP
# endpoint is on loopback and has NO authentication: put the sidecar in front
# of it (docs/installation.md) before anything but you can reach it. public is
# this server's own external URL, the base of upload links -- see
# docs/design/cimd-auth.md.
serve url token public addr="127.0.0.1:8081":
    HOMEBOX_URL={{url}} HOMEBOX_TOKEN={{token}} TRANSPORT=http ADDR={{addr}} \
        PUBLIC_URL={{public}} \
        go run ./cmd/homebox-mcp

# What the release will build, without publishing.
#
# KO_DOCKER_REPO is required even when publishing is skipped -- ko needs a
# repository to name the image it builds, and fails before building without
# one. CI passes the real registry; locally any name will do, and ko.local
# keeps the result out of a registry namespace that means something.
snapshot:
    KO_DOCKER_REPO=ko.local goreleaser release --snapshot --clean --skip=publish
