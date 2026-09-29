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

{{/* Refuse to render without the two facts the server cannot start without.
     A missing value here otherwise becomes a CrashLoopBackOff whose cause is
     one layer away in the pod log. */}}
{{- define "homebox-mcp.validate" -}}
{{- if not .Values.homebox.url -}}
{{- fail "homebox.url is required (base URL WITHOUT /api)" -}}
{{- end -}}
{{- if not .Values.homebox.existingSecret -}}
{{- fail "homebox.existingSecret is required: the API token is never a literal in values" -}}
{{- end -}}
{{- if not .Values.auth.issuerUrl -}}
{{- fail "auth.issuerUrl is required: the server validates every request against access-roster itself" -}}
{{- end -}}
{{- if not .Values.auth.resourceUrl -}}
{{- fail "auth.resourceUrl is required: it is this server's own external URL, the RFC 8707 audience access-roster mints tokens for" -}}
{{- end -}}
{{- end -}}
