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
{{- /* The release's own Secrets, by the names they RESOLVE to (nameOverride / fullnameOverride included), not by the values the operator typed: secrets.existingSecret, <fullname>-env (what x-monorepo.secretName picks when the chart renders it), and the hook-scoped <fullname>-migrate-env. catalog-rpc's FINAL Secret name (its own existingSecret, or the generated <fullname>-catalog-rpc-env) must be none of them, in either direction: a catalog existingSecret that points at a release Secret hands rpc's keys to catalog-rpc; a shared existingSecret that names catalog-rpc's generated Secret hands catalog-rpc's service key to rpc, worker and migrate, and the chart would render a Secret over the operator's. */ -}}
{{- $fn := include "x-monorepo.fullname" . -}}
{{- $final := include "x-monorepo.catalogRpc.secretName" . -}}
{{- $release := list .Values.secrets.existingSecret (include "x-monorepo.secretName" .) (printf "%s-env" $fn) (printf "%s-migrate-env" $fn) -}}
{{- if has $final $release -}}
{{- if $own -}}
{{- fail (printf "secrets.catalogRpc.existingSecret %q is one of the release-wide Secrets (they may carry rpc's JWT secret and user api keys, which catalog-rpc must not read); give catalog-rpc its own Secret" $final) -}}
{{- else -}}
{{- fail (printf "secrets.existingSecret %q is the name of the Secret the chart renders for catalog-rpc (<fullname>-catalog-rpc-env): rpc, worker and migrate would read catalog-rpc's service key, and the chart would render a Secret over yours; rename secrets.existingSecret" $final) -}}
{{- end -}}
{{- end -}}
{{- range $e := .Values.catalogRpc.extraEnv -}}
{{- $n := upper (toString $e.name) -}}
{{- if or (hasPrefix "APP_AUTH_" $n) (eq $n "APP_POSTGRES_URL") (eq $n "APP_CONFIG") -}}
{{- fail (printf "catalogRpc.extraEnv must not set %s: auth, catalog-rpc's Postgres URL and its config file come from the chart (an env var here would turn auth off or move the service off Postgres)" $e.name) -}}
{{- end -}}
{{- end -}}
{{- if $own -}}
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

{{/* catalog-rpc's own Secret. APP_CATALOG_API_KEY is rpc's copy of the SAME key (rpc reads only that one key from here, by secretKeyRef): one value feeds both sides, so they cannot drift. catalog-rpc itself reads APP_POSTGRES_URL and APP_AUTH_API_KEYS. */}}
{{- define "x-monorepo.catalogRpc.secretData" -}}
APP_POSTGRES_URL: {{ .Values.secrets.postgresUrl | quote }}
APP_AUTH_API_KEYS: {{ printf "rpc=%s:%s" .Values.secrets.catalogRpc.apiKey .Values.config.auth.admin_role | quote }}
APP_CATALOG_API_KEY: {{ .Values.secrets.catalogRpc.apiKey | quote }}
{{- end -}}

{{/*
rpc has no in-process catalog any more: it asks catalog-rpc (rpc.go: catalog.upstream + APP_CATALOG_API_KEY) and
does not start without them. So catalog-rpc is not optional, and what tells rpc where the catalog is and with which
key is the chart's, not the operator's extraEnv. The wait before rpc starts is best-effort (the binary logs a WARN and
starts anyway after its budget), but the budget itself must end before the Deployment gives up on the rollout.
*/}}
{{- define "x-monorepo.rpc.validateCatalog" -}}
{{- if not .Values.catalogRpc.enabled -}}
{{- fail "catalogRpc.enabled must be true: rpc has no in-process catalog to fall back on (it does not start without catalog-rpc)" -}}
{{- end -}}
{{- range $e := .Values.rpc.extraEnv -}}
{{- if hasPrefix "APP_CATALOG_" (upper (toString $e.name)) -}}
{{- fail (printf "rpc.extraEnv must not set %s: where rpc finds the catalog and the key it presents come from the chart (catalogRpc.*, secrets.catalogRpc.*)" $e.name) -}}
{{- end -}}
{{- end -}}
{{- $w := .Values.rpc.waitForCatalog -}}
{{- if and $w.enabled (ge (int $w.timeoutSeconds) (int .Values.rpc.progressDeadlineSeconds)) -}}
{{- fail (printf "rpc.waitForCatalog.timeoutSeconds (%d) must be < rpc.progressDeadlineSeconds (%d): the wait must end before the rollout is declared stuck" (int $w.timeoutSeconds) (int .Values.rpc.progressDeadlineSeconds)) -}}
{{- end -}}
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

{{/*
payment-rpc (apps/payment-rpc, docs/payment.md). One image, three workloads (serve, consume, expire), its own
ConfigMap and Secret: like catalog-rpc it refuses to start on auth.jwt / auth.oidc.enabled / auth on without
api_keys, and the release-wide config and Secret carry exactly those for rpc. Everything that could be
misconfigured fails here, at template time, before a pod can come up serving payments from memory or without
the service key; the binary's --require-durable (a container arg) is the second wall, on the final effective settings.
*/}}
{{- define "x-monorepo.paymentRpc.secretName" -}}
{{- default (printf "%s-payment-rpc-env" (include "x-monorepo.fullname" .)) .Values.secrets.paymentRpc.existingSecret -}}
{{- end -}}

{{/* Names a workload's env must not set (any case): what makes payment-rpc safe, and the provider's credentials. */}}
{{- define "x-monorepo.paymentRpc.guardedEnv" -}}
{{- $n := upper (toString .) -}}
{{- if or (hasPrefix "APP_AUTH_" $n) (hasPrefix "APP_PAYMENT_" $n) (hasPrefix "APP_WORKER_" $n) (hasPrefix "APP_REDIS_" $n) (eq $n "APP_POSTGRES_URL") (eq $n "APP_CONFIG") -}}
true
{{- end -}}
{{- end -}}

{{/* "30m" / "24h" / "1h30m" as a time after a fixed instant, so two durations compare with .Before. */}}
{{- define "x-monorepo.paymentRpc.duration" -}}
{{- $d := toString .d -}}
{{- if not (regexMatch "^([0-9]+(h|m|s))+$" $d) -}}
{{- fail (printf "paymentRpc.config.%s %q is not a duration like 30m, 24h or 1h30m" .name $d) -}}
{{- end -}}
{{- (toDate "2006-01-02" "2000-01-01" | dateModify $d).Unix -}}
{{- end -}}

{{- define "x-monorepo.paymentRpc.validate" -}}
{{- $p := .Values.paymentRpc -}}
{{- $s := .Values.secrets.paymentRpc -}}
{{- $own := $s.existingSecret -}}
{{- $fn := include "x-monorepo.fullname" . -}}
{{- $final := include "x-monorepo.paymentRpc.secretName" . -}}
{{- /* The three Secrets never alias each other, by the names they RESOLVE to, in either direction: payment-rpc's own (existingSecret or the generated <fullname>-payment-rpc-env) must be none of the release-wide ones nor catalog-rpc's; and nothing else may name the one the chart renders for payment-rpc. */ -}}
{{- $catalog := include "x-monorepo.catalogRpc.secretName" . -}}
{{- $release := list .Values.secrets.existingSecret (include "x-monorepo.secretName" .) (printf "%s-env" $fn) (printf "%s-migrate-env" $fn) $catalog -}}
{{- if $own -}}
{{- if has $final $release -}}
{{- fail (printf "secrets.paymentRpc.existingSecret %q is one of the release's other Secrets (the release-wide ones may carry rpc's JWT secret and user api keys, catalog-rpc's the key rpc presents to the catalog); give payment-rpc its own Secret" $final) -}}
{{- end -}}
{{- if $s.apiKey -}}
{{- fail "secrets.paymentRpc.existingSecret and secrets.paymentRpc.apiKey are both set: pick one source for payment-rpc's APP_AUTH_API_KEYS and APP_PAYMENT_API_KEY" -}}
{{- end -}}
{{- if $s.mockWebhookSecret -}}
{{- fail "secrets.paymentRpc.existingSecret and secrets.paymentRpc.mockWebhookSecret are both set: pick one source for payment-rpc's APP_PAYMENT_MOCK_WEBHOOK_SECRET" -}}
{{- end -}}
{{- else -}}
{{- if eq .Values.secrets.existingSecret $final -}}
{{- fail (printf "secrets.existingSecret %q is the name of the Secret the chart renders for payment-rpc (<fullname>-payment-rpc-env): rpc, worker and migrate would read payment-rpc's credentials, and the chart would render a Secret over yours; rename secrets.existingSecret" $final) -}}
{{- end -}}
{{- if eq .Values.secrets.catalogRpc.existingSecret $final -}}
{{- fail (printf "secrets.catalogRpc.existingSecret %q is the name of the Secret the chart renders for payment-rpc (<fullname>-payment-rpc-env): catalog-rpc would read payment-rpc's provider credential; rename secrets.catalogRpc.existingSecret" $final) -}}
{{- end -}}
{{- if not .Values.secrets.postgresUrl -}}
{{- fail "paymentRpc.enabled needs secrets.postgresUrl (or secrets.paymentRpc.existingSecret holding APP_POSTGRES_URL): without a Postgres URL payment-rpc would serve from memory and lose its payments on restart. secrets.existingSecret is the release-wide Secret and does not count: it may carry rpc's JWT secret, which payment-rpc refuses to start with" -}}
{{- end -}}
{{- $key := $s.apiKey -}}
{{- if not $key -}}
{{- fail "paymentRpc.enabled needs secrets.paymentRpc.apiKey (or secrets.paymentRpc.existingSecret holding APP_AUTH_API_KEYS and APP_PAYMENT_API_KEY): payment-rpc runs with auth.mode required and accepts only the key rpc presents" -}}
{{- end -}}
{{- if regexMatch "[,:=\\s]" $key -}}
{{- fail "secrets.paymentRpc.apiKey must not contain ',', ':', '=' or whitespace: it is embedded in APP_AUTH_API_KEYS (name=key:role,...)" -}}
{{- end -}}
{{- if not $s.mockWebhookSecret -}}
{{- fail "paymentRpc.enabled needs secrets.paymentRpc.mockWebhookSecret (or secrets.paymentRpc.existingSecret holding APP_PAYMENT_MOCK_WEBHOOK_SECRET): empty gives every process its own random secret, and serve could not verify what another process signed" -}}
{{- end -}}
{{- if regexMatch "\\s" $s.mockWebhookSecret -}}
{{- fail "secrets.paymentRpc.mockWebhookSecret must not contain whitespace" -}}
{{- end -}}
{{- end -}}
{{- /* extraEnv of the three workloads */ -}}
{{- range $wl := list "serve" "consume" "expire" -}}
{{- range $e := (index $p $wl).extraEnv -}}
{{- if include "x-monorepo.paymentRpc.guardedEnv" $e.name -}}
{{- fail (printf "paymentRpc.%s.extraEnv must not set %s: auth, payment-rpc's Postgres URL, its config file, the provider and its credentials, and the broker come from the chart (an env var here would turn one off, replace it, or hand a credential to a workload that must not have it)" $wl $e.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* serve: the Service carries the internal RPC on the webhook's port */ -}}
{{- if ne (toString $p.serve.service.type) "ClusterIP" -}}
{{- fail (printf "paymentRpc.serve.service.type must be ClusterIP, not %s: this port carries the internal Connect RPC as well as the webhook, and the RPC is for rpc alone" (toString $p.serve.service.type)) -}}
{{- end -}}
{{- $sd := $p.serve.shutdown -}}
{{- if lt (int $sd.delaySeconds) (int $p.serve.readiness.periodSeconds) -}}
{{- fail (printf "paymentRpc.serve.shutdown.delaySeconds (%d) must be >= paymentRpc.serve.readiness.periodSeconds (%d): endpoints must see the 503 before the listener closes" (int $sd.delaySeconds) (int $p.serve.readiness.periodSeconds)) -}}
{{- end -}}
{{- if lt (int $p.consume.terminationGracePeriodSeconds) 19 -}}
{{- fail (printf "paymentRpc.consume.terminationGracePeriodSeconds (%d) must be > 18: 8s handler drain + 10s router close" (int $p.consume.terminationGracePeriodSeconds)) -}}
{{- end -}}
{{- $iv := int $p.expire.intervalMinutes -}}
{{- if not (has $iv (list 1 2 3 4 5 6 10 12 15 20 30)) -}}
{{- fail (printf "paymentRpc.expire.intervalMinutes (%d) must be one of 1 2 3 4 5 6 10 12 15 20 30: the schedule is */N minutes and payment.expire_interval is Nm, both from this one value, so N has to divide the hour" $iv) -}}
{{- end -}}
{{- /* the numbers the binary checks at start, checked here too so the refusal is at template time */ -}}
{{- $c := $p.config -}}
{{- $win := include "x-monorepo.paymentRpc.duration" (dict "name" "window" "d" $c.window) | int64 -}}
{{- $rec := include "x-monorepo.paymentRpc.duration" (dict "name" "reconcile_window" "d" $c.reconcile_window) | int64 -}}
{{- $ttl := include "x-monorepo.paymentRpc.duration" (dict "name" "mock_checkout_ttl" "d" $c.mock_checkout_ttl) | int64 -}}
{{- $red := include "x-monorepo.paymentRpc.duration" (dict "name" "mock_redelivery_window" "d" $c.mock_redelivery_window) | int64 -}}
{{- $keepForever := has (toString $c.webhook_retention) (list "0" "0s" "0m" "0h") -}}
{{- $ret := 0 | int64 -}}
{{- if not $keepForever -}}
{{- $ret = include "x-monorepo.paymentRpc.duration" (dict "name" "webhook_retention" "d" $c.webhook_retention) | int64 -}}
{{- end -}}
{{- $base := (toDate "2006-01-02" "2000-01-01").Unix | int64 -}}
{{- if gt (sub $win $base) (sub $ttl $base) -}}
{{- fail (printf "paymentRpc.config.window (%s) must not be longer than paymentRpc.config.mock_checkout_ttl (%s): a payment has one checkout for its whole window, and payment-rpc refuses to start otherwise" $c.window $c.mock_checkout_ttl) -}}
{{- end -}}
{{- if le (sub $rec $base) (sub $ttl $base) -}}
{{- fail (printf "paymentRpc.config.reconcile_window (%s) must be longer than paymentRpc.config.mock_checkout_ttl (%s): a customer paying at the end of a page must be found by the sweep, and payment-rpc refuses to start otherwise" $c.reconcile_window $c.mock_checkout_ttl) -}}
{{- end -}}
{{- if and (not $keepForever) (le (sub $ret $base) (sub $red $base)) -}}
{{- fail (printf "paymentRpc.config.webhook_retention (%s) must be 0 (keep for ever) or longer than paymentRpc.config.mock_redelivery_window (%s): the webhook log is the dedup memory of redelivered notifications, and payment-rpc refuses to start otherwise" $c.webhook_retention $c.mock_redelivery_window) -}}
{{- end -}}
{{- if lt (int $c.expire_batch) 1 -}}
{{- fail "paymentRpc.config.expire_batch must be positive" -}}
{{- end -}}
{{- /* mock_control is written into payment-rpc's config.yaml as it is: a string "true" would not be the boolean the binary reads. */ -}}
{{- if not (kindIs "bool" (default false $c.mock_control)) -}}
{{- fail (printf "paymentRpc.config.mock_control must be true or false, not %v: it switches on the mock provider's control entry (payment-rpc mock pay|notify|fail), for kind and tests only" $c.mock_control) -}}
{{- end -}}
{{- /* the ingress: the webhook, rate limited, and nothing else */ -}}
{{- $i := $p.ingress -}}
{{- if $i.enabled -}}
{{- if not $i.className -}}
{{- fail "paymentRpc.ingress.className is required: the rate limit is rendered as ingress-nginx annotations, which no controller reads without a class that names one" -}}
{{- end -}}
{{- if not $i.hosts -}}
{{- fail "paymentRpc.ingress.hosts needs at least one host: a rule without a host would answer for every host the controller serves" -}}
{{- end -}}
{{- range $h := $i.hosts -}}
{{- if not $h.host -}}
{{- fail "paymentRpc.ingress.hosts[].host must be set" -}}
{{- end -}}
{{- end -}}
{{- if le (int $i.rateLimit.rps) 0 -}}
{{- fail "paymentRpc.ingress.rateLimit.rps must be positive: the webhook route has no API key and no http.rate_limit (docs/payment.md §7), so the ingress is the only place a flood of unsigned requests can be limited" -}}
{{- end -}}
{{- if le (int $i.rateLimit.burstMultiplier) 0 -}}
{{- fail "paymentRpc.ingress.rateLimit.burstMultiplier must be positive" -}}
{{- end -}}
{{- if le (int $i.rateLimit.connections) 0 -}}
{{- fail "paymentRpc.ingress.rateLimit.connections must be positive: a rate per second does not bound how many slow connections one client holds open" -}}
{{- end -}}
{{- range $k, $_ := $i.annotations -}}
{{- if or (hasPrefix "nginx.ingress.kubernetes.io/limit-" $k) (hasSuffix "-snippet" $k) (has $k (list "nginx.ingress.kubernetes.io/rewrite-target" "nginx.ingress.kubernetes.io/use-regex" "nginx.ingress.kubernetes.io/app-root" "nginx.ingress.kubernetes.io/proxy-body-size")) -}}
{{- fail (printf "paymentRpc.ingress.annotations must not set %s: the chart sets the limits, the body cap and the single exposed path; an annotation here could lift a limit or route more than the webhook" $k) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.paymentRpc.ingressFrom) -}}
{{- fail "networkPolicy.paymentRpc.ingressFrom is empty while paymentRpc.ingress is on: the provider's webhook arrives through the ingress controller, and payment-rpc serve admits only the rpc pods and what is listed there; list the controller's namespace" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* payment-rpc's config.yaml: the release's http / postgres / log / otel, auth required, the Postgres broker, payment.*. No auth.jwt / auth.oidc. otel.service_name is dropped (it would report as rpc; the binary defaults to payment-rpc). */}}
{{- define "x-monorepo.paymentRpc.config" -}}
{{- $c := .Values.config -}}
{{- $w := .Values.config.worker -}}
{{- toYaml (dict
      "http" $c.http
      "postgres" $c.postgres
      "log" $c.log
      "otel" (omit $c.otel "service_name")
      "auth" (dict "mode" "required" "admin_role" $c.auth.admin_role)
      "worker" (dict "broker" "postgres" "consumer_group" "payment-rpc" "start_from" $w.start_from "handler_timeout" $w.handler_timeout "poll_interval" $w.poll_interval)
      "payment" (merge (dict "provider" "mock" "expire_interval" (printf "%dm" (int .Values.paymentRpc.expire.intervalMinutes))) (deepCopy .Values.paymentRpc.config))) -}}
{{- end -}}

{{/* payment-rpc's own Secret. APP_PAYMENT_API_KEY is rpc's copy of the SAME key: one value feeds both sides, so they cannot drift. serve reads APP_POSTGRES_URL, APP_AUTH_API_KEYS and the mock secret; consume and expire only the Postgres URL and the mock secret (no listener, so no service key). */}}
{{- define "x-monorepo.paymentRpc.secretData" -}}
APP_POSTGRES_URL: {{ .Values.secrets.postgresUrl | quote }}
APP_AUTH_API_KEYS: {{ printf "rpc=%s:%s" .Values.secrets.paymentRpc.apiKey .Values.config.auth.admin_role | quote }}
APP_PAYMENT_API_KEY: {{ .Values.secrets.paymentRpc.apiKey | quote }}
APP_PAYMENT_MOCK_WEBHOOK_SECRET: {{ .Values.secrets.paymentRpc.mockWebhookSecret | quote }}
{{- end -}}

{{- define "x-monorepo.paymentRpc.checksums" -}}
checksum/config: {{ include "x-monorepo.paymentRpc.config" . | sha256sum }}
checksum/secrets: {{ include "x-monorepo.paymentRpc.secretData" . | sha256sum }}
{{- end -}}

{{/* The env a payment-rpc container reads from its Secret: `keys` is the list of the keys it needs (secretKeyRef, optional: false; never envFrom). */}}
{{- define "x-monorepo.paymentRpc.secretEnv" -}}
{{- $name := include "x-monorepo.paymentRpc.secretName" .root -}}
{{- range $k := .keys }}
- name: {{ $k }}
  valueFrom:
    secretKeyRef:
      name: {{ $name }}
      key: {{ $k }}
      optional: false
{{- end }}
{{- end -}}

{{- define "x-monorepo.paymentRpc.serve.terminationGracePeriodSeconds" -}}
{{- $s := .Values.paymentRpc.serve.shutdown -}}
{{- add (int $s.delaySeconds) (int $s.timeoutSeconds) (int $s.graceMarginSeconds) -}}
{{- end -}}

{{/* Where rpc reaches payment-rpc serve: its Service in this release, in-cluster. */}}
{{- define "x-monorepo.paymentRpc.upstream" -}}
{{- printf "http://%s-payment-serve:%d" (include "x-monorepo.fullname" .) (int .Values.paymentRpc.serve.service.port) -}}
{{- end -}}

{{/*
What the other workloads must not be told about payment. rpc is told where payment-rpc is and with which key
(APP_PAYMENT_UPSTREAM, APP_PAYMENT_API_KEY) by the chart — whether or not payment-rpc is on, so an extraEnv
cannot point rpc at something else or smuggle the key in — and the worker, which does not load payment, never
holds a provider credential. Also the one switch that makes orders wait for payment: on only with a payment-rpc
to pay at, and with auth on, because with auth off nobody can pay (docs/payment.md §6 (a)) and every order
would stay CONFIRMED.
*/}}
{{- define "x-monorepo.rpc.validatePayment" -}}
{{- range $e := .Values.rpc.extraEnv -}}
{{- if hasPrefix "APP_PAYMENT_" (upper (toString $e.name)) -}}
{{- fail (printf "rpc.extraEnv must not set %s: where rpc finds payment-rpc and the key it presents come from the chart (paymentRpc.*, secrets.paymentRpc.*)" $e.name) -}}
{{- end -}}
{{- end -}}
{{- range $e := .Values.worker.extraEnv -}}
{{- if hasPrefix "APP_PAYMENT_" (upper (toString $e.name)) -}}
{{- fail (printf "worker.extraEnv must not set %s: the worker does not load payment and never holds a provider credential" $e.name) -}}
{{- end -}}
{{- end -}}
{{- /* config.* is written verbatim into the release-wide ConfigMap, which rpc, the worker and the CronJobs mount: a payment credential there is readable by all of them. The binaries refuse it too (the worker and rpc at start); this is the render-time wall. */ -}}
{{- $cp := default dict .Values.config.payment -}}
{{- range $k := list "mock_webhook_secret" "api_key" -}}
{{- if index $cp $k -}}
{{- fail (printf "config.payment.%s must not be set: the release-wide ConfigMap is read by rpc, the worker and the CronJobs, and a payment credential does not belong in it; payment-rpc's own Secret (secrets.paymentRpc.*) carries it, and rpc gets the service key by APP_PAYMENT_API_KEY" $k) -}}
{{- end -}}
{{- end -}}
{{- $o := default dict .Values.config.ordering -}}
{{- if $o.require_payment -}}
{{- if not .Values.paymentRpc.enabled -}}
{{- fail "config.ordering.require_payment is true but paymentRpc.enabled is false: orders would wait for a payment nobody can start (rpc itself refuses to start without payment configured)" -}}
{{- end -}}
{{- if eq (toString .Values.config.auth.mode) "off" -}}
{{- fail "config.ordering.require_payment is true with config.auth.mode off: with auth off nobody can pay (no credential, no customer), so every order would stay CONFIRMED; set config.auth.mode to optional or required" -}}
{{- end -}}
{{- end -}}
{{- end -}}
