{{- define "homebox-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The standard Helm idiom, and not merely cosmetic: naively joining
     release and chart name yields homebox-mcp-homebox-mcp for the obvious
     release name, and a route pointing at "homebox-mcp" then fails with
     BackendNotFound -- which stays invisible behind a gateway that rejects
     unauthenticated requests before it ever routes them. */}}
{{- define "homebox-mcp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "homebox-mcp.labels" -}}
app.kubernetes.io/name: {{ include "homebox-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "homebox-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "homebox-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "homebox-mcp.proxyImage" -}}
{{- $i := .Values.proxy.image -}}
{{- printf "%s:%s@%s" $i.repository $i.tag $i.digest -}}
{{- end -}}

{{/*
The fixed addresses inside the pod.

  proxy inbound   :8080           the pod's only authenticated port
  server MCP      127.0.0.1:8081  loopback, no container port
  server upload   <pod IP>:8082   the signed upload link and nothing else
  server ops      :9090           /healthz, for the kubelet; in no Service
*/}}
{{- define "homebox-mcp.proxyPort" -}}8080{{- end -}}
{{- define "homebox-mcp.mcpListen" -}}127.0.0.1:8081{{- end -}}
{{- define "homebox-mcp.uploadPort" -}}8082{{- end -}}
{{- define "homebox-mcp.opsPort" -}}9090{{- end -}}

{{/* The path of auth.resourceUrl, no trailing slash. */}}
{{- define "homebox-mcp.resourcePath" -}}
{{- (urlParse .Values.auth.resourceUrl).path | trimSuffix "/" -}}
{{- end -}}

{{/* The host (no port) of auth.resourceUrl. */}}
{{- define "homebox-mcp.resourceHost" -}}
{{- regexReplaceAll ":[0-9]+$" (urlParse .Values.auth.resourceUrl).host "" -}}
{{- end -}}

{{/* The port of a URL, from its scheme when it names none. */}}
{{- define "homebox-mcp.urlPort" -}}
{{- $u := urlParse . -}}
{{- $p := regexFind ":[0-9]+$" $u.host | trimPrefix ":" -}}
{{- default (ternary "80" "443" (eq $u.scheme "http")) $p -}}
{{- end -}}

{{/*
Every refusal in this chart. They guard a value that is EMPTY (or wrong) by
default and whose absence renders cleanly and fails later, somewhere else: a
pod that crash-loops with the cause one layer away, or one that serves a
resource nobody can get a token for.
*/}}
{{- define "homebox-mcp.validate" -}}
{{- if not .Values.homebox.url -}}
{{- fail "homebox.url is required (base URL WITHOUT /api)" -}}
{{- end -}}
{{- if not .Values.homebox.existingSecret -}}
{{- fail "homebox.existingSecret is required: the API token is never a literal in values" -}}
{{- end -}}
{{- if not .Values.auth.issuerUrl -}}
{{- fail "auth.issuerUrl is required: the resource-proxy sidecar validates every caller's access token against the issuer's signing keys and `iss`; without it the pod would be an unauthenticated door to the inventory" -}}
{{- end -}}
{{- if not (hasPrefix "https://" .Values.auth.issuerUrl) -}}
{{- fail (printf "auth.issuerUrl %q must be an https URL" .Values.auth.issuerUrl) -}}
{{- end -}}
{{- if not .Values.auth.resourceUrl -}}
{{- fail "auth.resourceUrl is required: it is this server's own external URL, the RFC 8707 audience the proxy requires of every token, and the base of upload links" -}}
{{- end -}}
{{- if not (hasPrefix "https://" .Values.auth.resourceUrl) -}}
{{- fail (printf "auth.resourceUrl %q must be an https URL" .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if or (contains "?" .Values.auth.resourceUrl) (contains "#" .Values.auth.resourceUrl) -}}
{{- fail (printf "auth.resourceUrl %q must have no query or fragment" .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if hasSuffix "/" .Values.auth.resourceUrl -}}
{{- fail (printf "auth.resourceUrl %q must have no trailing slash: it is compared with a token's `aud` byte for byte, and clients are given it without one" .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if not (include "homebox-mcp.resourcePath" .) -}}
{{- fail (printf "auth.resourceUrl %q has no path. The proxy maps the path to the server's /mcp and the HTTPRoute matches it as a prefix; a bare host would take the whole host. Use a path such as https://mcp.example/homebox." .Values.auth.resourceUrl) -}}
{{- end -}}
{{- if not .Values.proxy.image.tag -}}
{{- fail "proxy.image.tag is empty. The resource-proxy sidecar is built by another repository's release, so this chart carries no default version; pin the release you have verified." -}}
{{- end -}}
{{- if not .Values.proxy.image.digest -}}
{{- fail "proxy.image.digest is empty. The sidecar checks every token and is pinned by tag AND digest, so a moved tag cannot change what guards the server." -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.ingressFrom) -}}
{{- fail "networkPolicy.enabled is true but networkPolicy.ingressFrom is empty, so the rendered policy would admit NOBODY to the proxy and upload ports, the gateway included. Name the gateway's peers, or set networkPolicy.enabled: false if the cluster's CNI does not enforce NetworkPolicy." -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.httpRoute.parentRefs -}}
{{- fail "httpRoute.enabled is true but httpRoute.parentRefs is empty, so the route would attach to no Gateway and answer nothing. Name the Gateway it attaches to." -}}
{{- end -}}
{{- if and .Values.httpRoute.hostnames (not (has (include "homebox-mcp.resourceHost" .) .Values.httpRoute.hostnames)) -}}
{{- fail (printf "the host %q of auth.resourceUrl is not in httpRoute.hostnames (%s); the route would not match the URL clients are told to use." (include "homebox-mcp.resourceHost" .) (join ", " .Values.httpRoute.hostnames)) -}}
{{- end -}}
{{- /* A Gateway API duration is whole hours, minutes, seconds and milliseconds. */ -}}
{{- if not (regexMatch "^([0-9]{1,5}(h|m|s|ms)){1,4}$" (toString .Values.upload.timeout)) -}}
{{- fail (printf "upload.timeout %q cannot be a Gateway API request timeout (whole h, m, s, ms such as 15m or 1h30m); the upload route takes its timeout from it" .Values.upload.timeout) -}}
{{- end -}}
{{- end -}}
{{- end -}}
