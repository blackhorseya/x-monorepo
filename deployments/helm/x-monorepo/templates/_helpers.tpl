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

{{/* Labels. component: rpc | worker | migrate | prune */}}
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
