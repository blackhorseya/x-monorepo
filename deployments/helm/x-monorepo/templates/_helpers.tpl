{{/* Names */}}
{{- define "x-monorepo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "x-monorepo.fullname" -}}
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

{{- define "x-monorepo.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "x-monorepo.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Labels. component: rpc | catalog-rpc | worker | migrate | prune */}}
{{- define "x-monorepo.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "x-monorepo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "x-monorepo.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "x-monorepo.selectorLabels" -}}
app.kubernetes.io/name: {{ include "x-monorepo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Images */}}
{{- define "x-monorepo.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}

{{- define "x-monorepo.image" -}}
{{- printf "%s/%s:%s" .root.Values.image.registry .app (include "x-monorepo.imageTag" .root) -}}
{{- end -}}

{{/* Secret the app pods read APP_* secrets from (empty when nothing is configured). */}}
{{- define "x-monorepo.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else if (include "x-monorepo.secretData" .) -}}
{{- include "x-monorepo.fullname" . }}-env
{{- end -}}
{{- end -}}

{{/* The non-empty secret keys as `KEY: value` lines; empty string when there are none. */}}
{{- define "x-monorepo.secretData" -}}
{{- with .Values.secrets -}}
{{- if .postgresUrl }}
APP_POSTGRES_URL: {{ .postgresUrl | quote }}
{{- end }}
{{- if .redisUrl }}
APP_REDIS_URL: {{ .redisUrl | quote }}
{{- end }}
{{- if .apiKeys }}
APP_AUTH_API_KEYS: {{ .apiKeys | quote }}
{{- end }}
{{- if .jwtHs256Secret }}
APP_AUTH_JWT_HS256_SECRET: {{ .jwtHs256Secret | quote }}
{{- end }}
{{- end -}}
{{- end -}}

{{/* Env every app container gets: the config file, then the secrets. */}}
{{- define "x-monorepo.commonEnv" -}}
env:
  - name: APP_CONFIG
    value: /etc/x-monorepo/config.yaml
{{- with .extra }}
{{ toYaml . | indent 2 }}
{{- end }}
{{- with (include "x-monorepo.secretName" .root) }}
envFrom:
  - secretRef:
      name: {{ . }}
{{- end }}
{{- end -}}

{{- define "x-monorepo.configVolume" -}}
volumes:
  - name: config
    configMap:
      name: {{ include "x-monorepo.fullname" . }}
{{- end -}}

{{- define "x-monorepo.configMount" -}}
volumeMounts:
  - name: config
    mountPath: /etc/x-monorepo
    readOnly: true
{{- end -}}

{{/* Roll pods when config or secrets change. */}}
{{- define "x-monorepo.checksums" -}}
checksum/config: {{ toYaml .Values.config | sha256sum }}
checksum/secrets: {{ include "x-monorepo.secretData" . | sha256sum }}
{{- end -}}

{{/* Derived shutdown numbers for rpc. Validation lives here so every consumer agrees. */}}
{{- define "x-monorepo.rpc.terminationGracePeriodSeconds" -}}
{{- $s := .Values.rpc.shutdown -}}
{{- if lt (int $s.delaySeconds) (int .Values.rpc.readiness.periodSeconds) -}}
{{- fail (printf "rpc.shutdown.delaySeconds (%d) must be >= rpc.readiness.periodSeconds (%d): endpoints must see the 503 before the listener closes" (int $s.delaySeconds) (int .Values.rpc.readiness.periodSeconds)) -}}
{{- end -}}
{{- add (int $s.delaySeconds) (int $s.timeoutSeconds) (int $s.graceMarginSeconds) -}}
{{- end -}}

{{- define "x-monorepo.worker.terminationGracePeriodSeconds" -}}
{{- $t := int .Values.worker.terminationGracePeriodSeconds -}}
{{- if lt $t 19 -}}
{{- fail (printf "worker.terminationGracePeriodSeconds (%d) must be > 18: 8s handler drain + 10s router close" $t) -}}
{{- end -}}
{{- $t -}}
{{- end -}}

{{/* Where the console's nginx sends /api: the rpc Service of this release, in-cluster. */}}
{{- define "x-monorepo.console.rpcUpstream" -}}
{{- printf "http://%s-rpc:%d" (include "x-monorepo.fullname" .) (int .Values.rpc.service.port) -}}
{{- end -}}

{{/*
catalog-rpc (apps/catalog-rpc) authenticates static auth.api_keys only and refuses to start on
auth.jwt, auth.oidc.enabled, or auth on without api_keys. So it never reads the release-wide
ConfigMap and Secret (they carry rpc's JWT secret, OIDC issuers and user api keys): it has its
own, built here. Rendered only when catalogRpc.enabled; every misconfiguration fails at template
time, before a pod can come up in the in-memory mode an unset Postgres URL would give it.
*/}}
{{- define "x-monorepo.catalogRpc.validate" -}}
{{- $own := .Values.secrets.catalogRpc.existingSecret -}}
{{- $key := .Values.secrets.catalogRpc.apiKey -}}
{{- range $e := .Values.catalogRpc.extraEnv -}}
{{- $n := upper (toString $e.name) -}}
{{- if or (hasPrefix "APP_AUTH_" $n) (eq $n "APP_POSTGRES_URL") (eq $n "APP_CONFIG") -}}
{{- fail (printf "catalogRpc.extraEnv must not set %s: auth, catalog-rpc's Postgres URL and its config file come from the chart (an env var here would turn auth off or move the service off Postgres)" $e.name) -}}
{{- end -}}
{{- end -}}
{{- if $own -}}
{{- if eq $own .Values.secrets.existingSecret -}}
{{- fail "secrets.catalogRpc.existingSecret is the same Secret as secrets.existingSecret: that one is release-wide and may carry rpc's JWT secret, which catalog-rpc refuses to start with; give catalog-rpc its own Secret" -}}
{{- end -}}
{{- if $key -}}
{{- fail "secrets.catalogRpc.existingSecret and secrets.catalogRpc.apiKey are both set: pick one source for catalog-rpc's APP_AUTH_API_KEYS" -}}
{{- end -}}
{{- else -}}
{{- if not .Values.secrets.postgresUrl -}}
{{- fail "catalogRpc.enabled needs secrets.postgresUrl (or secrets.catalogRpc.existingSecret holding APP_POSTGRES_URL): without a Postgres URL catalog-rpc would serve from memory and lose its prices on restart. secrets.existingSecret is the release-wide Secret and does not count: it may carry rpc's JWT secret, which catalog-rpc refuses to start with" -}}
{{- end -}}
{{- if not $key -}}
{{- fail "catalogRpc.enabled needs secrets.catalogRpc.apiKey (or secrets.catalogRpc.existingSecret holding APP_AUTH_API_KEYS): catalog-rpc runs with auth.mode required and accepts only this key" -}}
{{- end -}}
{{- if regexMatch "[,:=\\s]" $key -}}
{{- fail "secrets.catalogRpc.apiKey must not contain ',', ':', '=' or whitespace: it is embedded in APP_AUTH_API_KEYS (name=key:role,...)" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "x-monorepo.catalogRpc.secretName" -}}
{{- default (printf "%s-catalog-rpc-env" (include "x-monorepo.fullname" .)) .Values.secrets.catalogRpc.existingSecret -}}
{{- end -}}

{{/* catalog-rpc's config.yaml: the release's http / postgres / log / otel, auth required, and no auth.jwt / auth.oidc. otel.service_name is dropped (it would report as rpc; the binary defaults to catalog-rpc). */}}
{{- define "x-monorepo.catalogRpc.config" -}}
{{- $c := .Values.config -}}
{{- toYaml (dict
      "http" $c.http
      "postgres" $c.postgres
      "log" $c.log
      "otel" (omit $c.otel "service_name")
      "auth" (dict "mode" "required" "admin_role" $c.auth.admin_role)) -}}
{{- end -}}

{{- define "x-monorepo.catalogRpc.secretData" -}}
APP_POSTGRES_URL: {{ .Values.secrets.postgresUrl | quote }}
APP_AUTH_API_KEYS: {{ printf "rpc=%s:%s" .Values.secrets.catalogRpc.apiKey .Values.config.auth.admin_role | quote }}
{{- end -}}

{{- define "x-monorepo.catalogRpc.checksums" -}}
checksum/config: {{ include "x-monorepo.catalogRpc.config" . | sha256sum }}
checksum/secrets: {{ include "x-monorepo.catalogRpc.secretData" . | sha256sum }}
{{- end -}}

{{- define "x-monorepo.catalogRpc.terminationGracePeriodSeconds" -}}
{{- $s := .Values.catalogRpc.shutdown -}}
{{- if lt (int $s.delaySeconds) (int .Values.catalogRpc.readiness.periodSeconds) -}}
{{- fail (printf "catalogRpc.shutdown.delaySeconds (%d) must be >= catalogRpc.readiness.periodSeconds (%d): endpoints must see the 503 before the listener closes" (int $s.delaySeconds) (int .Values.catalogRpc.readiness.periodSeconds)) -}}
{{- end -}}
{{- add (int $s.delaySeconds) (int $s.timeoutSeconds) (int $s.graceMarginSeconds) -}}
{{- end -}}

{{/* Where rpc reaches catalog-rpc (the forwarding slice): the catalog-rpc Service of this release, in-cluster. */}}
{{- define "x-monorepo.catalogRpc.upstream" -}}
{{- printf "http://%s-catalog-rpc:%d" (include "x-monorepo.fullname" .) (int .Values.catalogRpc.service.port) -}}
{{- end -}}
