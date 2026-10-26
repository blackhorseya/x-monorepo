# Operations：Compose、k6、Helm

<sub>← [README](../README.md)</sub>

## Local infra（Compose）

```bash
task compose-up      # podman compose（沒有 podman 就 docker compose）起 Postgres（:5432，user/pass/db 都是 app）＋ otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up   # 自動 seed 的 configs/local.yaml 已把 postgres.url 指向它，不用 export
task run             # app 在本機跑，不進容器
task worker          # 事件 consumer process（另開一個 terminal）
task compose-down    # 連 volume 一起清
task grafana-reload  # 改了 deployments/docker/grafana/{dashboards/*.json,provisioning/alerting.yaml} 之後
```

**Grafana dashboard 由 repo provision**：`deployments/docker/grafana/provisioning/dashboards.yaml` 掛進 otel-lgtm 的
provisioning 目錄，`deployments/docker/grafana/dashboards/*.json` 掛到 `/otel-lgtm/dashboards`，開起來就在 folder
`x-monorepo` 裡（`allowUiUpdates: false`，檔案是 source of truth：UI 改完 Export → 覆蓋 JSON → commit）。
`rpc.json`（「x-monorepo / service」）以 `job` = `service.name` 當變數，換 BC / app 不用改 panel：

| row | 來源 | 看什麼 |
|---|---|---|
| Overview | otelconnect + runtime | req/s、error ratio、p95、instance 數 |
| RPC | `rpc_server_call_duration_seconds` | 各 method 的流量、非 `OK` 的 status code、p50/p95/p99 |
| Use cases | `usecase_duration_seconds` | 各 usecase 流量、非 `ok` 的 `kind`、p95 |
| Worker — eventbus | `eventbus_consumer_lag`、`eventbus_consumer_oldest_age_seconds`、`eventbus_consumer_duration_seconds`、`eventbus_poison_messages` | 每個 consumer group（含 forwarder）的 backlog 與最老訊息年齡、handler attempt/s（含 retry）、poison 表大小；`$service` 選 `worker` |
| Worker — eventbus（第二排） | `eventbus_outbox_rows`、`eventbus_outbox_held`、`eventbus_inbox_rows`、`eventbus_outbox_oldest_age_seconds`、`eventbus_outbox_oldest_held_age_seconds`、`eventbus_inbox_oldest_age_seconds` | prune 管的表：大小、被 group 扣住的列、最老一列的年齡（超過 retention = prune 沒跑或刪不動；held 的年齡一直長 = stale group） |
| Saga | `ordering_orders{status}`、`inventory_reservations{status}`、`*_oldest_age_seconds{status}` | ordering ↔ inventory 的漏斗：各 status 幾筆、最老一筆卡多久；PENDING 不消而 CONFIRMED / COMPLETED / CANCELLED 不動就是 saga 斷了；CONFIRMED 一直不變 COMPLETED / CANCELLED 是業務面（沒人按 Complete），不是 bug；`$service` 選 `worker` |
| HTTP routes | `http_server_request_duration_seconds`（otelgin） | `/api/*` 的 route / status、p95 |
| Auth | `auth_decisions_total{result, method}`、`rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}` | 每秒多少 ok / public / anonymous / missing / invalid，各用什麼 credential；被拒的三種（沒帶、帶錯、role 不夠）疊在一起看。counter，`sum` |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTel runtime instrumentation 的名字，OTLP 與 `/api/metrics` 相同。`/api/metrics` 用自建 registry，沒有 client_golang 的 `go_memstats_*`、`process_*` |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

Alert rules 也從 repo provision（`deployments/docker/grafana/provisioning/alerting.yaml` → folder `x-monorepo`，每分鐘評估；group
`worker-eventbus` 全部用 `max by`，gauge 是 per replica 不能 sum；group `rpc-auth` 讀 counter，跨 replica `sum`）：

| rule | 條件 | 意思 / 怎麼處理 |
|---|---|---|
| Eventbus consumer lag | `eventbus_consumer_lag` > 100 持續 5m | handler 追不上或沒人在消費；看 oldest age 分辨，再加 worker replica |
| Eventbus consumer stalled | `eventbus_consumer_oldest_age_seconds` > 600 持續 5m | 該 group 完全沒前進（通常是 subscriber 連不到 DB / Redis，不是 handler 失敗——失敗的會進 poison） |
| Eventbus poison messages | `eventbus_poison_messages` > 0 持續 1m | 有訊息停在 poison 表，`task poison -- list` 看、`requeue` / `drop` 解，alert 隨 gauge 歸零 |
| Eventbus retention overdue | `eventbus_outbox_oldest_age_seconds` / `eventbus_inbox_oldest_age_seconds` > 45d 持續 1h | prune 沒在跑，或跑了刪不動；`task prune -- --older-than 720h --dry-run` 或 console Prune 頁的 dry run 會說是誰擋的。閾值設成 prune retention 的 1.5× |
| Eventbus rows held by a stale consumer group | `eventbus_outbox_oldest_held_age_seconds` > 1d 持續 30m | offsets 表裡有 group 一天沒前進；consumer stalled 沒響就是沒人跑的 group（舊 `worker.consumer_group`、E2E 的 `test-<ts>.*`），`--forget-group` / console Prune 頁的 forget 刪掉它的 cursor |
| Saga stuck — order PENDING too long | `ordering_orders_oldest_age_seconds{status="PENDING"}` > 600 持續 5m | 有訂單等 inventory 的答覆超過 10 分鐘：看 `ordering.v1.OrderCreated`（inventory 的 group）與 `inventory.v1.StockReserved`（ordering 的 group）的 lag，再看 poison 表——handler 一直 drop 的話 eventbus 幾條都不會紅，只有這條會 |
| Eventbus metrics absent | `absent(eventbus_consumer_lag)` 持續 10m | 沒有 worker 在匯出，上面幾條都是瞎的 |
| Auth credential rejections | `rate(auth_decisions_total{result="invalid"}[5m])` > 1/s 持續 5m（by `method`） | 有人在猜 key / 重放 token，或 key 換了 client 還送舊的；Loki `{service_name="rpc"} \| code="unauthenticated"` 看是哪個 procedure、request id（credential 本身不記）。rate limit 在 auth 外層，洪水先被擋 |
| Auth permission denied | `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}[5m])` > 0.1/s 持續 10m（by `rpc_method`） | 驗過身分但 role 不夠（`auth.permission_denied`）：access line 有 `sub`，是正當 operator 就補 role（`name=key:role` / JWT `roles`），不是就是在摸 admin surface |

每條都帶 `__dashboardUid__` / `__panelId__`，所以「Worker — eventbus」與「Auth」的 panel 上會畫 alert 狀態。只有 rule 沒有送達：contact point /
notification policy 由部署環境依 `severity` label 自己接（Grafana → Alerting → Notification policies）。Alerting provisioning
只在啟動時讀，改完一樣 `task grafana-reload`。

踩雷：Grafana 13 的 file provisioner 用 inotify 監看資料夾，podman machine / Docker Desktop 的 bind mount 在 macOS 上
不會把事件傳進容器，所以改 JSON 不會自動重載 → `task grafana-reload`。另外 OTLP 的 metric export 間隔 60s，
`rate()` 剛啟動前兩分鐘是空的。`compose-down -v` 會清掉 Grafana 的 DB，但 provisioning 會重建 dashboard。
Retention 預設 72h（compose 的 `LGTM_RETENTION`，透過 image 的 `LOKI/TEMPO/PROMETHEUS_EXTRA_ARGS` 下 flag；Pyroscope v2 storage
還沒有時間型 retention）。Loki 突然沒 log 但 metrics / traces 正常，先看 `podman machine ssh df -h /`：VM 磁碟快滿時 Loki 的
WAL 會判 disk full、對每個 push 回 503 整批丟掉（`loki_ingester_wal_disk_full_failures_total`），跟 app 無關，`podman system prune` 就好。

## Load test（k6）

`tests/k6/` 是四個對著真的 `rpc` 打的 k6 腳本（Connect over JSON，`lib.js` 的 `rpc()` 按 method 打 tag，`API_KEY` 有設就帶 `X-API-Key`）：

| 腳本 | 打什麼 | 驗什麼 |
|---|---|---|
| `orders.js` | setup 先 `SetStock` 20 個 SKU，每個 VU 迴圈 CreateOrder（1–3 行）→ GetOrder → ListOrders(customer) | `http_req_failed < 1%`、CreateOrder p95 < 300ms / p99 < 800ms、GetOrder p95 < 100ms、ListOrders p95 < 200ms、`rpc_errors == 0`（門檻是「筆電 + Docker Postgres 輕鬆過」的數字，要調是刻意調，不是失敗了改） |
| `ratelimit.js` | server 開 `http.rate_limit.rps=RPS`，以 `RPS × FACTOR`（預設 4）的 constant-arrival-rate 打 ListOrders | 被拒的每一筆都是 429 + `code: resource_exhausted` + `x-error-code: request.rate_limited` + 整數 `Retry-After ≥ 1`；放行數 ≈ `RPS × 秒數`（+ 一個 burst）、其餘全拒——limiter 是拒絕不是排隊 |
| `timeout.js` | server 開 `http.timeout=1ns`（要有 `postgres.url`：context 過期是在 DB call 才浮上來，in-memory repo 不看它） | 每一筆都是 504 + `code: deadline_exceeded` + `x-error-code: request.timeout`，不是 INTERNAL（pgx 包過的 context error 也要被對應到）；server 沒斷線 |
| `reservation.js` | 旁邊要有一個 `worker`（同一個 Postgres）：每個 VU CreateOrder（1 行、qty 1）後每 `POLL`（100ms）打一次 GetReservation，404 視為「還沒消費」 | `reservation_latency`（CreateOrder 回來 → 看到 RESERVED）p95 < `RESERVE_P95`（預設 3000ms；outbox 是 polling，約 1s 在等下一個 tick，筆電實測 p50 0.7s / p95 1.03s）、`RESERVE_TIMEOUT`（30s）內沒等到算 `reservation_timeouts`、不是 RESERVED 算 `reservation_wrong_status`，兩者都要 0 |

```bash
task k6 -- orders                 # 對 http://127.0.0.1:8080（task run）跑 60s × 20 VU；BASE_URL / API_KEY / VUS / DURATION / RPS 用 env 蓋
task k6 -- ratelimit -e RPS=50    # server 要先用 APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50 起
task k6 -- reservation            # server 旁要有 bin/worker 在跑（同一個 Postgres），不然每筆都 timeout
task k6-smoke                     # tests/k6/smoke.sh：四個腳本各自起一個 bin/rpc（:18090、local Postgres、對應的 env；reservation 排第一且多起一個 bin/worker，讓它面對空的 outbox；RUN_ID 每個腳本不同，SKU 才不會互撞），10s × 10 VU；CI 的 k6 job 就是它；CreateOrder 的 p95 門檻放寬到 CREATE_P95（預設 500ms；腳本本身預設 300）、reservation 的放寬到 RESERVE_P95（預設 5000ms；腳本 3000），shared runner 的尾巴比筆電寬
```

worker 有跑時每張訂單都會讓 inventory 去 reserve，`orders.js` 不等它——要看 consumer 追不追得上就看 `eventbus_consumer_lag`；這也是 Helm `worker.autoscaling.metric: lag` 的壓力源。`reservation.js` 才等：它量的是單筆訂單從下單到 RESERVED 的端到端延遲，是 worker 鏈的 benchmark 基準。

## Deploy（Helm）

`deployments/helm/x-monorepo/` 是這些 image 的 chart，沒有 dependency、不假設 ingress controller 或 secret 工具。它做的事是把
README 的 server / Events 節講的「部署要配合的行為」寫成算式，而不是讓人手抄：

| 物件 | 怎麼接 app |
|---|---|
| ConfigMap `config.yaml` | `values.config` 原樣 render（key 同 `configs/config.example.yaml`），`APP_CONFIG` 指過去；預設 `http.docs: false`、`log.format: json`、`auth.mode: off` |
| Secret `<release>-env` | `secrets.{postgresUrl,redisUrl,apiKeys,jwtHs256Secret}` → `APP_POSTGRES_URL` / `APP_REDIS_URL` / `APP_AUTH_API_KEYS` / `APP_AUTH_JWT_HS256_SECRET`（env 蓋過檔案）；或 `secrets.existingSecret` 用自己管的 |
| rpc Deployment | liveness `/api/healthz`、readiness `/api/readyz`；`rpc.shutdown.{delaySeconds,timeoutSeconds}` 變成 `APP_HTTP_SHUTDOWN_*`，`terminationGracePeriodSeconds = delay + timeout + graceMargin`，`readiness.periodSeconds > delaySeconds` 直接 render 失敗。image 是 distroless 沒 shell，所以沒有 `preStop: sleep`——app 自己的 `shutdown_delay` 就是那個 sleep |
| worker Deployment | **沒有 probe**（worker 沒 HTTP server；起不來會 exit 非 0 由 Deployment 重啟，健康看 `eventbus_*` gauge 與 alert）；`terminationGracePeriodSeconds` 預設 30，< 19 render 失敗 |
| migrate Job | `pre-install,pre-upgrade` hook 跑 `migrate up`：新 code 不會先於新 schema 起來。hook 跑在 release 的 Secret 存在之前，所以 chart 另 render 一個 hook-scoped 的 `<release>-migrate-env`（跑完刪）；`existingSecret` 則直接讀它 |
| prune CronJob | `worker prune --older-than <prune.olderThan>`（預設 720h、每天一次、`concurrencyPolicy: Forbid`）；`prune.forgetGroups` 對應 `--forget-group`。alert「retention overdue」的 45d 是 720h 的 1.5×，改一個要一起改 |
| HPA / PDB / Ingress | rpc：CPU HPA（開了就不 render `replicas`、要有 `resources.requests.cpu`）、PDB 只在可能 >1 replica 時 render、Ingress 走 HTTP/1.1（Connect / gRPC-Web），原生 gRPC 要另外 h2c |
| console Deployment（`console.enabled`，預設 off） | `web/apps/console` 的 nginx image：唯一的 env 是 `RPC_UPSTREAM=http://<release>-rpc:<rpc.service.port>`（chart 算出來），**沒有** `APP_CONFIG`、沒有 Secret——nginx 看不到 Postgres URL 和 API key，auth 開著時使用者在 console 的 Credential 對話框貼自己的 key / JWT；probe 打 nginx 自己的 `/healthz`（rpc 的健康是 rpc probe 的事）；同一組 `podSecurityContext`（uid 65532、read-only rootfs）+ `/tmp` emptyDir 就夠。自己的 Service / Ingress（`console.ingress`，純 HTTP/1.1） |
| ServiceMonitor（`rpc.serviceMonitor.enabled`，預設 off） | Prometheus Operator 抓 rpc Service 的 `http` port、path `/api/metrics`（gin route，auth 開著也不擋）；operator 靠 label 選 ServiceMonitor，`serviceMonitor.labels` 放它要的（kube-prometheus-stack 是 `release: <name>`）。只管 rpc——worker 沒 HTTP server，它的 metrics 只能走 `config.otel.endpoint` |
| NetworkPolicy（`networkPolicy.enabled`，預設 off） | 每個 Deployment 一張、只管 ingress（egress 不碰：DB / Redis / collector 在哪是 cluster 的事）。rpc 永遠放行同 release 的 console pod，加上 `networkPolicy.rpc.ingressFrom`（NetworkPolicyPeer list；**空 = 該 port 對任何來源開**，要收緊就列 ingress controller 與 Prometheus 的 namespace）；worker 不收任何 ingress；console 同一套 `console.ingressFrom`。要 CNI 真的實作 NetworkPolicy 才有效，kind 預設的 kindnet 不會，所以 helm-kind 驗不了，check.py 驗形狀 |
| worker autoscaling | replica 共用 `consumer_group` 就是分工，但 handler 是 DB-bound，CPU 低時 lag 照樣長，所以 `worker.autoscaling.metric` 預設 `lag`：KEDA `ScaledObject` 用 Prometheus trigger 查 `max(eventbus_consumer_lag{consumer_group=~"<group>\\..*"})`（gauge 是 per replica，取 max 不 sum；要 `config.otel.endpoint`，否則拒絕 render）；沒 KEDA 的 cluster 用 `metric: cpu` 出普通 HPA。實測踩到的三件事都做成 render 時就擋：`lag.prometheusAddress` 要 `http://<svc>.<ns>.svc:<port>`（KEDA operator 從**它自己的** namespace 解析，裸 Service 名不通）；`minReplicas` 必須 ≥ 1（lag gauge 是 worker 自己報的，0 個 replica 就沒人報）；`pollingInterval` / `cooldownPeriod` 只對 scale-to-zero 有效，縮回去的節奏是 HPA 的 `behavior.scaleDown`（預設 stabilization 300s，`worker.autoscaling.behavior` 透傳） |

```bash
task helm-check    # 每個 ci/*.yaml profile：helm lint --strict → helm template | kubeconform -strict → deployments/helm/check.py
helm install api deployments/helm/x-monorepo -n app --create-namespace \
  --set-string secrets.postgresUrl='postgres://…' --set config.otel.endpoint=otel-collector:4317   # image 是 public 的 ghcr，不用 pull secret
```

`check.py` 是 chart 裡唯一有邏輯的地方的測試：rpc 的 grace period 算式與 env、readiness period 的關係、worker 沒 probe、migrate hook
與它的 Secret 的順序、prune 的 args、console 的 `RPC_UPSTREAM` 指到這個 release 的 rpc Service 且沒有 `APP_*` / Secret。ServiceMonitor 選到 rpc Service 的 port、NetworkPolicy 的 peers（console 一定在、`ingressFrom` 原樣、空就不出 `from`）。`ci/` 六個 profile（預設 + DB、redis broker + HPA + ingress + auth required、existingSecret、無 DB + 沒列 peer 的 NetworkPolicy、console + 自己的 ingress、hardened：NetworkPolicy 列 ingress-nginx / monitoring + ServiceMonitor + console）
在 CI 的 `helm` job 跑。真的裝一次是 `task helm-kind`（CI 的 `helm-kind` job）：起 kind cluster、`deployments/helm/kind/deps.yaml`
放一個拋棄式 Postgres + Redis（chart 沒有 subchart dependency，production 接 managed DB，這份只給 smoke 用）、從 working tree build 三個 Go image + console image（`web-build` → nginx）
`kind load` 進去、`helm upgrade --install --wait` 兩次（第一次走 pre-install hook、第二次走 pre-upgrade），每次跑 `kind/smoke.sh`：
SetStock → CreateOrder → worker 透過 Redis Streams 消費 OrderCreated → GetStock 看到 reserved；console 的 `/reservations` 回 `index.html`、`/api/readyz` 經 nginx proxy 到 rpc。本機約 1 分鐘（cluster 已存在時），`task helm-kind-down` 刪 cluster。
接著 `task helm-kind-keda`（CI 同一個 job 的第二步）：裝 KEDA、`kind/monitoring.yaml` 放一個 OTLP collector（gRPC :4317 → Prometheus exporter :8889）
和一個 5s scrape 的 Prometheus，用 `kind/keda-values.yaml` 重裝 chart（`OTEL_METRIC_EXPORT_INTERVAL=5000`、threshold 50、scaleDown stabilization 30s），
`kind/scale.sh` 用 `tests/k6/orders.js` 灌 45 秒訂單：實測 lag 0 → 2000+，worker 13 秒內 1 → 4 replica，backlog 消化完縮回 1。
`appVersion`（image.tag 空時用它）由 release.yml 的 `chart` job 在每次發版後開一支 chart-only PR 跟上（bot push 不會觸發 CI，所以它自己 `gh workflow run ci.yml`；要 repo 開「Allow GitHub Actions to create and approve pull requests」，沒開就只推 branch、summary 給 compare 連結）；chart 本身沒有 publish 到 OCI registry。
