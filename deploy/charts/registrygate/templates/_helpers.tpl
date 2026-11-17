{{- define "registrygate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "registrygate.fullname" -}}
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

{{- define "registrygate.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "registrygate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "registrygate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "registrygate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "registrygate.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "registrygate.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The rendered proxy config file: the server's own settings. Upstreams, credentials, mirrors and access come from the custom resources. */}}
{{- define "registrygate.config" -}}
authentication:
  audience: {{ .Values.authentication.audience | quote }}
  cacheTTL: {{ .Values.authentication.cacheTTL | quote }}
  reviewTimeout: {{ .Values.authentication.reviewTimeout | quote }}
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
{{- if .Values.webhook.enabled }}
webhook:
  enabled: true
  listen: ":9443"
  certFile: /var/run/registrygate/webhook-tls/tls.crt
  keyFile: /var/run/registrygate/webhook-tls/tls.key
{{- end }}
{{- end }}

{{/* Name of the Secret with the webhook certificate. */}}
{{- define "registrygate.webhookSecretName" -}}
{{- if .Values.webhook.certManager.issuerRef.name -}}
{{- printf "%s-webhook-tls" (include "registrygate.fullname" .) -}}
{{- else -}}
{{- required "webhook.existingSecret (or webhook.certManager.issuerRef.name) is required when webhook.enabled" .Values.webhook.existingSecret -}}
{{- end -}}
{{- end }}

{{/* Grace period: shutdownGracePeriod plus 30 seconds. Accepts one number and a unit: 90s, 10m or 1h. */}}
{{- define "registrygate.terminationGracePeriodSeconds" -}}
{{- $d := toString .Values.server.shutdownGracePeriod -}}
{{- $m := regexFindAll "^[0-9]+[smh]$" $d 1 -}}
{{- if not $m -}}{{- fail "server.shutdownGracePeriod must be a whole number and one unit: s, m or h (for example 10m)" -}}{{- end -}}
{{- $n := trimSuffix "s" (trimSuffix "m" (trimSuffix "h" $d)) | int -}}
{{- $mult := 1 -}}
{{- if hasSuffix "m" $d -}}{{- $mult = 60 -}}{{- end -}}
{{- if hasSuffix "h" $d -}}{{- $mult = 3600 -}}{{- end -}}
{{- add (mul $n $mult) 30 -}}
{{- end }}

{{/* Name of the Secret with the upload signing key. */}}
{{- define "registrygate.uploadSigningSecretName" -}}
{{- default (printf "%s-upload-signing" (include "registrygate.fullname" .)) .Values.uploadSigning.existingSecret -}}
{{- end }}
