{{- define "ghcr-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "ghcr-proxy.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "ghcr-proxy.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "ghcr-proxy.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "ghcr-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ghcr-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "ghcr-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ghcr-proxy.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The rendered proxy config file. The Secrets are mounted as directories under /var/run/ghcr-proxy. */}}
{{- define "ghcr-proxy.config" -}}
upstream:
  url: {{ .Values.upstream.url | quote }}
  tokenHosts: {{ .Values.upstream.tokenHosts | toJson }}
  patFile: /var/run/ghcr-proxy/pat/token
  {{- if .Values.upstream.ca.existingSecret }}
  caFile: /var/run/ghcr-proxy/upstream-ca/ca.crt
  {{- end }}
  timeout: {{ .Values.upstream.timeout | quote }}
  tokenTimeout: {{ .Values.upstream.tokenTimeout | quote }}
authentication:
  audience: {{ .Values.authentication.audience | quote }}
  cacheTTL: {{ .Values.authentication.cacheTTL | quote }}
  reviewTimeout: {{ .Values.authentication.reviewTimeout | quote }}
grants: {{ .Values.grants | toJson }}
tls:
  certFile: /var/run/ghcr-proxy/tls/tls.crt
  keyFile: /var/run/ghcr-proxy/tls/tls.key
listen:
  https: ":8443"
  health: ":8080"
server:
  readHeaderTimeout: {{ .Values.server.readHeaderTimeout | quote }}
  idleTimeout: {{ .Values.server.idleTimeout | quote }}
  maxHeaderBytes: {{ .Values.server.maxHeaderBytes | int }}
  shutdownGracePeriod: {{ .Values.server.shutdownGracePeriod | quote }}
audit:
  enabled: {{ .Values.audit.enabled }}
{{- end }}

{{/* Grace period: shutdownGracePeriod plus 30 seconds. Accepts one number and a unit: 90s, 10m or 1h. */}}
{{- define "ghcr-proxy.terminationGracePeriodSeconds" -}}
{{- $d := toString .Values.server.shutdownGracePeriod -}}
{{- $m := regexFindAll "^[0-9]+[smh]$" $d 1 -}}
{{- if not $m -}}{{- fail "server.shutdownGracePeriod must be a whole number and one unit: s, m or h (for example 10m)" -}}{{- end -}}
{{- $n := trimSuffix "s" (trimSuffix "m" (trimSuffix "h" $d)) | int -}}
{{- $mult := 1 -}}
{{- if hasSuffix "m" $d -}}{{- $mult = 60 -}}{{- end -}}
{{- if hasSuffix "h" $d -}}{{- $mult = 3600 -}}{{- end -}}
{{- add (mul $n $mult) 30 -}}
{{- end }}
