# Operations：Compose、Helm

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
| HTTP routes | `http_server_request_duration_seconds`（otelgin） | `/api/*` 的 route / status、p95 |
| Auth | `auth_decisions_total{result, method}`、`rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}` | 每秒多少 ok / public / anonymous / missing / invalid，各用什麼 credential；被拒的三種（沒帶、帶錯、role 不夠）疊在一起看。counter，`sum` |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTel runtime instrumentation 的名字，OTLP 與 `/api/metrics` 相同。`/api/metrics` 用自建 registry，沒有 client_golang 的 `go_memstats_*`、`process_*` |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

Alert rules 也從 repo provision（`deployments/docker/grafana/provisioning/alerting.yaml` → folder `x-monorepo`，每分鐘評估；group
`worker-eventbus` 全部用 `max by`，gauge 是 per replica 不能 sum；group `rpc-auth` 讀 counter，跨 replica `sum`）：

| rule | 條件 | 意思 / 怎麼處理 |
|---|---|---|
| Eventbus consumer lag | `eventbus_consumer_lag` > 100 持續 5m | handler 追不上或沒人在消費；看 oldest age 分辨，處置見「Worker 輪詢間隔」的「要不要調」（加 replica 是否有效取決於 broker，那裡有說明） |
| Eventbus consumer stalled | `eventbus_consumer_oldest_age_seconds` > 600 持續 5m | 該 group 完全沒前進（通常是 subscriber 連不到 DB / Redis，不是 handler 失敗——失敗的會進 poison） |
| Eventbus poison messages | `eventbus_poison_messages` > 0 持續 1m | 有訊息停在 poison 表，`task poison -- list` 看、`requeue` / `drop` 解，alert 隨 gauge 歸零 |
| Eventbus retention overdue | `eventbus_outbox_oldest_age_seconds` / `eventbus_inbox_oldest_age_seconds` > 45d 持續 1h | prune 沒在跑，或跑了刪不動；`task prune -- --older-than 720h --dry-run` 或 console Prune 頁的 dry run 會說是誰擋的。閾值設成 prune retention 的 1.5× |
| Eventbus rows held by a stale consumer group | `eventbus_outbox_oldest_held_age_seconds` > 1d 持續 30m | offsets 表裡有 group 一天沒前進；consumer stalled 沒響就是沒人跑的 group（舊 `worker.consumer_group`、E2E 的 `test-<ts>.*`），`--forget-group` / console Prune 頁的 forget 刪掉它的 cursor |
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

## Worker 輪詢間隔（`worker.poll_interval`）

`postgres` broker 的 handler 靠輪詢 outbox 表拿事件（watermill-sql 的 `PollInterval`；redis 模式只有 forwarder 的 SQL 端輪詢）。watermill-sql 只有在一次查詢**沒拿到訊息**之後才等滿一個間隔，所以這個間隔是閒置時的**上限**、不是下限：新事件在等待期間任何時刻到達，額外延遲落在 0 到約一個間隔之間；找到訊息時 subscriber 不會等，下一次查詢立刻發出，所以忙碌時看到的是 handler 本身的速度。排程、DB 與 handler 處理仍會再加上延遲。實測：保留 library 預設 1s、空 poll 後 900ms 才 publish，事件約 104ms 就送達（watermill-sql v4.1.5 `pkg/sql/backoff_manager.go`：只有 noMsg 才回傳 `pollInterval`）。

預設 200ms（舊預設是 library 的 1s）：間隔是閒置時新事件等待的上限，也決定閒置時對 Postgres 的輪詢頻率。

**成本怎麼算**：每個輪詢迴圈每個間隔一次輪詢（一個短交易，約 3 條 statement：offsets `FOR UPDATE`、取訊息、更新 cursor），所以

```
輪詢 xact/s ≈ 輪詢迴圈數 × replica 數 ÷ 間隔(秒)
輪詢迴圈數 = postgres broker：所有 BC 的 handler subscription 數
           = redis 模式：forwarder 對每個 distinct forwarded topic 各 Subscribe 一次（apps/worker/app.go），一個 topic 一個迴圈
```

目前主線沒有 BC 有 consumer，輪詢迴圈數是 0；有 consumer 之後照上式算：例如 5 個輪詢迴圈、200ms、2 個 replica ≈ 50 xact/s；KEDA 把 worker 撐到 10 個 replica 就是 250 xact/s（1s 時是 50）。這些輪詢是索引上的小查詢，但它們佔連線、也佔 `max_connections`——調小間隔前先算好 replica 上限。

**要不要調**：看 `eventbus_consumer_lag` 與 `eventbus_consumer_oldest_age_seconds`。
- 平常 lag 為 0、`oldest_age` 幾乎都在一個間隔之內：剩下的主要延遲就是輪詢等待（最多約一個間隔），要更低才縮間隔（如 100ms）。
- lag 持續 > 0 且 `oldest_age` 遠大於間隔：瓶頸是 handler 吞吐，不是輪詢；縮間隔沒用，先看 handler。`postgres` broker 下加 replica 是否有效取決於 broker：每個 replica 的 cursor 列是 `FOR UPDATE`（同一個 topic 的輪詢要輪流領），這是對機制的推演，沒有量過；要靠加 replica 擴充，改用 `worker.broker: redis`（本文沒有量 redis 模式的吞吐）。
- Postgres 的 `pg_stat_database.xact_commit` 在閒置時就很高、或連線數吃緊：放大間隔（如 500ms–1s），延遲換 DB 負載。
重送（nack）與查詢失敗後的等待仍是 library 的 1s：handler 失敗由 `Register` 的 Retry 在行程內重試，不靠 nack；DB 出錯時也不該更頻繁地打它。

設定：`worker.poll_interval` / `APP_WORKER_POLL_INTERVAL`（Helm `config.worker.poll_interval`），必須 > 0。

## Deploy（Helm）


### Upgrade notes — chart 0.3.79：移除 identity bounded context（#561）

這一版把 identity bounded context 從 template 移除：`pkgs/identity`、`protos/identity`、它的 migration、`worker bootstrap`（含 `task bootstrap`）、Console 的 identity 頁與 OIDC 登入（#562）、k6 測試與 CI job、Grafana 的 identity 面板列與告警。**不寫 DROP migration**（既有資料庫怎麼辦見下）。platform 的擴充點（OIDC verifier、`server.CredentialResolver`、`server.ExternalIdentityResolver`）保留。

- **既有資料庫**：template 不寫 DROP migration。升級後 `identity` schema 與 `identity.schema_migrations` 原樣留著，沒有任何程式再讀寫它。實測：新 `migrate` 的清單是空的，`up`／`down`／`version` 對留著 `identity` schema 的資料庫都不輸出、不報錯、不碰它（`apps/migrate/main_test.go` 的 `TestRun_LeavesAnUnknownSchemaAlone`）；新 `worker` 對這樣的資料庫正常啟動、收到 SIGTERM 正常結束（發版前在 PR 分支用一次性資料庫手動測，沒有留成自動測試）；新 rpc 對這樣的資料庫**未實測**。不需要這些資料就自己備份後移除（`DROP SCHEMA identity CASCADE;` 是常見寫法，**本 repo 沒有驗過**）；要從乾淨狀態開始就重建資料庫。新安裝不會建立 `identity` schema。
- **行為**：
  - rpc 不再有 `identity.v1`（帳號、API key 管理、外部身分綁定、邀請、撤銷、`WhoAmI` 都沒有了，路由已不存在，呼叫不會成功）；`worker bootstrap` 與 `task bootstrap` 移除（`worker bootstrap` 現在是 `unknown command`）。靜態 `auth.api_keys` 與 `auth.jwt` 是僅有的兩種內建憑證。
  - **`auth.mode` 是 `optional` 或 `required` 而 `auth.api_keys`、`auth.jwt` 都沒設，rpc 現在拒絕啟動**（以前靠 identity 資料庫裡的 key 就能起來）。
  - **`auth.oidc.enabled: true` 現在在啟動時被拒絕**（`auth.mode: off` 也一樣）：template 的 rpc 沒有接 `server.ExternalIdentityResolver`，錯誤訊息指向那個 interface 與 `apps/rpc/wire.go`。要用外部 OIDC 登入，自己實作並在 `apps/rpc` 的 wire 接上（`apps/rpc/startup_test.go` 逐格釘住）。
  - `poison`／`prune` 在沒有事件 BC 時沒有資料可看。
- **Helm values**：`config.auth.last_used_interval` 已移除；舊 values 帶著它不會報錯（新 binary 對 env `APP_AUTH_LAST_USED_INTERVAL` 與設定檔的 `auth.last_used_interval` 兩種都忽略、照常啟動），可以拿掉。
- **監控**：`identity_resolver_*` 指標、告警 group `rpc-identity`（Identity resolver errors）與 Grafana 的「Identity」面板列移除；JWKS stale 告警與「Auth」面板列保留（它們量的是 platform 的 verifier 與 auth interceptor）。
- **k6**：`tests/k6/`、`task k6*` 與 CI 的 `k6`、`k6-auth`、`k6-wire` job 移除（#561 的決定：template 不附壓測腳本）。
- **image**：第一個不含 identity 的 image tag 預計是 v0.70.0，**尚未發布**。
- **回滾界線**：只寫實測到的。實測：新 binary 帶著舊 values 的 `auth.last_used_interval` 照常啟動，所以 chart 0.3.78 的 values 配新 image 在這一項不會被擋；`identity` schema 沒被新 `migrate` 動過（上面的測試），所以回退到含 identity 的版本時它的資料還在。**未實測**：舊 chart 的 migrate hook 配新 migrate、舊 ConfigMap 的其他鍵、舊版 migrate／rpc／worker 接回這個 schema 後能否正常運作。

### Upgrade notes — chart 0.3.73：移除 catalog 與 catalog-rpc（#518 切片 2）

這一版把 catalog bounded context 與 `apps/catalog-rpc` 從 repo 移除（程式、proto、Helm、console、k6、CI 與文件）。**不寫 DROP migration**，所以升級本身不會動到既有資料；要注意的是下面幾件事。

- **資料庫裡的 schema 原樣留下**：已部署環境的 `catalog` schema（`catalog.products`、`catalog.schema_migrations`）不會被刪；新安裝不會建立它（`apps/migrate` 不再跑 catalog 的 migration）。
- **要清除時是手動步驟**：先備份，確認沒有任何程式還在讀它，再執行：

```sql
DROP SCHEMA catalog CASCADE;
```

- **回滾界線**：這份 notes 只涵蓋回滾到上一版 chart 0.3.72（v0.67.9）：它需要 catalog-rpc 與 `catalog` schema。若已 `DROP SCHEMA catalog`，該版的 catalog-rpc 與 rpc 仍然起得來、readiness 也會是 200（啟動與 readiness 只 ping Postgres，不查 `catalog.products`），但第一次讀寫 catalog 才會因缺表失敗，rpc 對 catalog 的呼叫可能回 `catalog.unavailable`（catalog-rpc 5xx 會被映射成這個；實際取決於 catalog-rpc 缺表時怎麼失敗，本次沒有實測）。所以健康檢查通過不代表回滾成功；要讓 catalog 請求恢復，必須先還原 schema 與資料（從 DROP 前的備份）。清除 schema 之前，先確認不需要回滾。回滾到更早的 chart 不在這份 notes 的範圍：拓樸隨版本不同（例如已核對的 0.3.35〔v0.53.0〕，catalog 在 rpc 行程內、沒有 catalog-rpc；其他版本沒有核對，不要假設），請看該版本自己的文件。
- **以 Helm 部署時**：`helm upgrade` 到 0.3.73，chart 之前 render 過的 catalog-rpc 資源（Deployment、Service、ConfigMap；依當時的 values 還有 `<release>-catalog-rpc-env` Secret〔沒設 `secrets.catalogRpc.existingSecret` 才有〕、PDB〔`replicaCount` > 1 且開啟才有〕、ServiceMonitor〔`catalogRpc.serviceMonitor.enabled` 才有〕、NetworkPolicy〔`networkPolicy.enabled` 才有〕）因為模板移除，由 Helm 刪除。**不是 Helm 管的東西不會被動**，需要手動清理：
  - 自己管的 Secret（`secrets.catalogRpc.existingSecret` 指的那個）。
  - 沒有走這個 chart 部署的 catalog-rpc Deployment（以及它的 Service 等）。
  - 舊的 image tag：`ghcr.io/blackhorseya/x-monorepo/catalog-rpc:*` 的既有 tag 不受影響，之後不再發佈。

```bash
# 先列出 chart 渲染過、還留在叢集裡的（正常升級後應該是空的），確認清單再刪
kubectl -n <ns> get deploy,svc,cm,secret,pdb,servicemonitor,networkpolicy -l app.kubernetes.io/component=catalog-rpc
# 自己管的 Secret 要用名字刪
kubectl -n <ns> delete secret <name>
```

- **Helm values**：`catalogRpc`、`secrets.catalogRpc`、`rpc.waitForCatalog`、`networkPolicy.catalogRpc`、`config.catalog` 已移除；升級時請從自己的 values 拿掉（留著只是無效鍵）。`APP_CATALOG_*` 環境變數不再被讀取。
- **行為**：rpc 不再有 `catalog.v1`（`UpsertProduct`／`GetProduct`／`ListProducts`／`DeactivateProduct`／`QuotePrices` 都沒有了，這些 procedure 的路由已不存在，呼叫不會成功），console 的 Products 頁（`/products`）移除。rpc 不再需要 catalog upstream 即可啟動；`rpc --wait-catalog` 與 initContainer `wait-for-catalog` 移除，rpc 現在只有 `--version`／`-v` 一個參數。
- **錯誤碼**：`catalog.unavailable` 與 `product.*` 移除（`docs/error-codes.md` 同步）。
- **trace**：目前沒有任何服務信任遠端 trace（`server.TrustRemoteTrace` 沒有使用者，函式仍在 `pkgs/platform/server`）；rpc 一直不延續外部的 `traceparent`，見 `docs/observability.md`。
- **kind**：`helm-kind-core` 不再跑 `negative.sh`／`outage.sh`（只有 install → smoke → upgrade → smoke）；`ci-negative/` 目錄清空，`check-negative.sh` 移除，`task helm-check` 不再跑它。

### Upgrade notes — chart 0.3.71：移除 ordering、inventory、payment（#518 切片 1）

這一版把 ordering、inventory、payment 三個 bounded context 從 repo 移除（程式、proto、Helm、console、k6、CI 與文件）。**不寫 DROP migration**，所以升級本身不會動到既有資料；要注意的是下面幾件事。

- **資料庫裡的 schema 原樣留下**：已部署環境的 `ordering`、`inventory`、`payment` 三個 schema（含表、type，以及各自的 events／offsets／poison／inbox 表）不會被刪；新安裝不會建立它們（`apps/migrate` 不再跑這三個 BC 的 migration）。
- **要清除時是手動步驟**：先備份，確認沒有任何程式或排程還在讀它們，再執行：

```sql
DROP SCHEMA ordering CASCADE;
DROP SCHEMA inventory CASCADE;
DROP SCHEMA payment CASCADE;
```

- **Redis broker**：若使用 `worker.broker: redis`，名為 `ordering.v1.*`、`inventory.v1.*`、`payment.v1.*` 的 Redis Stream 與 consumer group 也會留下，需手動 `DEL`；先列出（例如 `redis-cli --scan --pattern 'ordering.v1.*'`）確認清單，再刪。
- **Helm values**：`config.ordering`、`secrets.paymentRpc`、`paymentRpc`、`networkPolicy.paymentRpc`、`expire` 已移除；升級時請從自己的 values 拿掉（留著只是無效鍵）。`APP_ORDERING_*`／`APP_PAYMENT_*` 環境變數不再被讀取。`payment-rpc`、`payment-worker` image 不再發佈（既有 tag 不受影響）。`worker expire`、`task expire` 子命令已移除。
- **migrate**：`apps/migrate` 的 `migrate version`／`down` 輸出不再包含這三個 schema。
- **Grafana**：Saga／Payment 面板列與 saga-pending、reservation-expiry、commit-short、payment-* alert 已移除；eventbus／auth／identity 的面板與 alert 保留（identity 的在 chart 0.3.79 移除）。
- **依 consumer lag 擴縮 worker**：目前沒有載具（沒有 BC 發 event）；ScaledObject 的 render 與 schema 由 `task helm-check` 的 `ci/redis-values.yaml` 驗（沒有叢集裝 KEDA，不驗 KEDA 是否接受它）；#518「已知缺口」未定案。

`deployments/helm/x-monorepo/` 是這些 image 的 chart，沒有 dependency、不假設 ingress controller 或 secret 工具。它做的事是把
README 的 server / Events 節講的「部署要配合的行為」寫成算式，而不是讓人手抄：

| 物件 | 怎麼接 app |
|---|---|
| ConfigMap `config.yaml` | `values.config` 原樣 render（key 同 `configs/config.example.yaml`），`APP_CONFIG` 指過去；預設 `http.docs: false`、`log.format: json`、`auth.mode: off` |
| Secret `<release>-env` | `secrets.{postgresUrl,redisUrl,apiKeys,jwtHs256Secret}` → `APP_POSTGRES_URL` / `APP_REDIS_URL` / `APP_AUTH_API_KEYS` / `APP_AUTH_JWT_HS256_SECRET`（env 蓋過檔案）；或 `secrets.existingSecret` 用自己管的 |
| rpc Deployment | liveness `/api/healthz`、readiness `/api/readyz`；`rpc.shutdown.{delaySeconds,timeoutSeconds}` 變成 `APP_HTTP_SHUTDOWN_*`，`terminationGracePeriodSeconds = delay + timeout + graceMargin`，`readiness.periodSeconds > delaySeconds` 直接 render 失敗。image 是 distroless 沒 shell，所以沒有 `preStop: sleep`——app 自己的 `shutdown_delay` 就是那個 sleep。 |
| worker Deployment | **沒有 probe**（worker 沒 HTTP server；起不來會 exit 非 0 由 Deployment 重啟，健康看 `eventbus_*` gauge 與 alert）；`terminationGracePeriodSeconds` 預設 30，< 19 render 失敗 |
| migrate Job | `pre-install,pre-upgrade` hook 跑 `migrate up`：新 code 不會先於新 schema 起來。hook 跑在 release 的 Secret 存在之前，所以 chart 另 render 一個 hook-scoped 的 `<release>-migrate-env`（跑完刪）；`existingSecret` 則直接讀它 |
| prune CronJob | `worker prune --older-than <prune.olderThan>`（預設 720h、每天一次、`concurrencyPolicy: Forbid`）；`prune.forgetGroups` 對應 `--forget-group`。alert「retention overdue」的 45d 是 720h 的 1.5×，改一個要一起改 |
| auth.oidc（`config.auth.oidc`） | 外部 OIDC token verifier 的 values，原樣進 ConfigMap：`enabled`（預設 `false`，關著時整段不讀）、`issuers[]`（`issuer` https、`audience`＝我們的 client id、`mapping` `bound`\|`auto_provision`、dev 才用的 `allow_insecure_issuer`）。**chart 不含 IdP**——Keycloak / Auth0 / Entra 自備；`auth.jwt` 也有設時，`auth.jwt.issuer` 要釘住且不得等於任一 OIDC issuer，否則 Pod 起不來。list 不能用 env 覆蓋，只能寫在 values。**template 的 rpc 沒有接 `server.ExternalIdentityResolver`，`enabled: true` 會在啟動時被拒絕**（見 chart 0.3.79 的 upgrade notes）；要用就自己實作並在 `apps/rpc` 的 wire 接上。verifier 的規則見 `docs/identity-login.md`（讀法見該文開頭） |
| HPA / PDB / Ingress | rpc：CPU HPA（開了就不 render `replicas`、要有 `resources.requests.cpu`）、PDB 只在可能 >1 replica 時 render、Ingress 走 HTTP/1.1（Connect / gRPC-Web），原生 gRPC 要另外 h2c |
| console Deployment（`console.enabled`，預設 off） | `web/apps/console` 的 nginx image：chart 算出來的 env 只有 `RPC_UPSTREAM=http://<release>-rpc:<rpc.service.port>`，**沒有** `APP_CONFIG`、沒有 Secret——nginx 看不到 Postgres URL 和 API key；auth 開著時使用者在 console 的 Credential 控制項貼自己的 key / JWT（console 沒有登入頁，也不讀任何執行期設定：沒有 `/config.json`，CSP 的 `connect-src` 固定是 `'self'`）。**升級注意**：以前走 `console.extraEnv` 的 `CONSOLE_OIDC_ISSUER`、`CONSOLE_OIDC_CLIENT_ID`、`CONSOLE_API_KEY_LOGIN` 現在沒人讀，設了也不會報錯，沒有效果，可以從 values 拿掉；probe 打 nginx 自己的 `/healthz`（rpc 的健康是 rpc probe 的事）；同一組 `podSecurityContext`（uid 65532、read-only rootfs）+ `/tmp` emptyDir 就夠。自己的 Service / Ingress（`console.ingress`，純 HTTP/1.1） |
| ServiceMonitor（`rpc.serviceMonitor.enabled`，預設 off） | Prometheus Operator 抓 rpc Service 的 `http` port、path `/api/metrics`（gin route，auth 開著也不擋）；operator 靠 label 選 ServiceMonitor，`serviceMonitor.labels` 放它要的（kube-prometheus-stack 是 `release: <name>`）。只管 rpc——worker 沒 HTTP server，它的 metrics 只能走 `config.otel.endpoint` |
| NetworkPolicy（`networkPolicy.enabled`，預設 off） | 每個 Deployment 一張、只管 ingress（egress 不碰：DB / Redis / collector 在哪是 cluster 的事）。rpc 永遠放行同 release 的 console pod，加上 `networkPolicy.rpc.ingressFrom`（NetworkPolicyPeer list；**空 = 該 port 對任何來源開**，要收緊就列 ingress controller 與 Prometheus 的 namespace）；worker 不收任何 ingress；console 同一套 `console.ingressFrom`。要 CNI 真的實作 NetworkPolicy 才有效，kind 預設的 kindnet 不會，所以 helm-kind 驗不了，check.py 驗形狀 |
| worker autoscaling | replica 共用 `consumer_group` 就是分工（加 replica 是否有效取決於 broker，見「Worker 輪詢間隔」的「要不要調」），但 handler 是 DB-bound，CPU 低時 lag 照樣長，所以 `worker.autoscaling.metric` 預設 `lag`：KEDA `ScaledObject` 用 Prometheus trigger 查 `max(eventbus_consumer_lag{consumer_group=~"<group>\\..*"})`（gauge 是 per replica，取 max 不 sum；要 `config.otel.endpoint`，否則拒絕 render）；沒 KEDA 的 cluster 用 `metric: cpu` 出普通 HPA。實測踩到的三件事都做成 render 時就擋：`lag.prometheusAddress` 要 `http://<svc>.<ns>.svc:<port>`（KEDA operator 從**它自己的** namespace 解析，裸 Service 名不通）；`minReplicas` 必須 ≥ 1（lag gauge 是 worker 自己報的，0 個 replica 就沒人報）；`pollingInterval` / `cooldownPeriod` 只對 scale-to-zero 有效，縮回去的節奏是 HPA 的 `behavior.scaleDown`（預設 stabilization 300s，`worker.autoscaling.behavior` 透傳） |

**Migration 通則**：migration 在**一個交易**裡跑；`ALTER TABLE` 取 ACCESS EXCLUSIVE lock 並持有到整個 migration commit，該表的**讀與寫**都會等到它結束，大表請排維護窗口（窗口要涵蓋讀流量）。順序是先 migrate 再升 rpc／worker（chart 的 `pre-upgrade` hook 已保證）。golang-migrate 在執行檔案之前就把 `<schema>.schema_migrations` 標成 `<N> dirty=true`，所以失敗之後每次 `migrate up` 都拒絕（`Dirty database version N`）：排除原因後把版本設回 `N-1` clean 再重跑。**不要用 `task migrate -- down` 退單一 BC**——它對每個 BC 各退一步（每個 BC 的最新 migration 都會回滾）；只退一個 BC 時，在該 BC 的資料庫以專屬的 rollback runbook 執行（`BEGIN; …; UPDATE <schema>.schema_migrations SET version = <N-1>, dirty = false; COMMIT;`）。目前沒有 BC 有 rollback runbook 案例，新增時的要求見 `docs/workflow.md`「單一 BC 的 rollback runbook」。

**Secret 不要印在叢集的 Pod stdout**：叢集裡任何 Pod（Job、`kubectl run`、`kubectl exec`）的 stdout 都會進 Pod log，也可能被 log collector 收走。會輸出 secret 的操作者指令只在自己的 terminal 跑，經 `kubectl port-forward`（或原本連 DB 的 proxy / bastion）連 DB，不要寫成叢集裡的 Job。port-forward 在有些環境（kind + podman 實測）會在 client 連線被重置後自己結束（`lost connection to pod`）；重跑時出現 `connection refused` 就重開 port-forward。

```bash
task helm-check    # 每個 ci/*.yaml profile：helm lint --strict → helm template | kubeconform -strict → deployments/helm/check.py
helm install api deployments/helm/x-monorepo -n app --create-namespace \
  --set-string secrets.postgresUrl='postgres://…' \
  --set config.otel.endpoint=otel-collector:4317   # image 是 public 的 ghcr，不用 pull secret
```

`check.py` 是 chart 裡唯一有邏輯的地方的測試：rpc 的 grace period 算式與 env、readiness period 的關係、worker 沒 probe、migrate hook
與它的 Secret 的順序、prune 的 args、console 的 `RPC_UPSTREAM` 指到這個 release 的 rpc Service 且沒有 `APP_*` / Secret。ServiceMonitor 選到 rpc Service 的 port、NetworkPolicy 的 peers（console 一定在、`ingressFrom` 原樣、空就不出 `from`）。`ci/` 五個 profile（預設 + DB；redis broker + HPA + ingress + auth required + worker 以 lag 擴縮（ScaledObject，kubeconform 用 CRDs-catalog 的 `keda.sh` schema 驗）；existingSecret；console + 自己的 ingress；hardened：NetworkPolicy 列 ingress-nginx / monitoring + ServiceMonitor + console）。
在 CI 的 `helm` job 跑。真的裝一次是 `task helm-kind`（CI 的 `helm-kind` job）：起 kind cluster、`deployments/helm/kind/deps.yaml`
放一個拋棄式 Postgres + Redis（chart 沒有 subchart dependency，production 接 managed DB，這份只給 smoke 用）、從 working tree build 三個 Go image（rpc / worker / migrate）+ console image（`web-build` → nginx）
`kind load` 進去、`helm upgrade --install --wait` 兩次（第一次走 pre-install hook、第二次走 pre-upgrade），每次跑 `kind/smoke.sh`：
rpc 的 `/api/readyz` 與 `/api/version`；console 回 `index.html`、`/api/readyz` 經 nginx proxy 到 rpc。本機約 1 分鐘（cluster 已存在時），`task helm-kind-down` 刪 cluster。
沒有叢集裝 KEDA：worker 以 lag 擴縮的 ScaledObject 只在 `helm` job 驗 render 與 schema，KEDA operator 是否接受它、`--wait` 是否成功、OTLP collector 到 Prometheus 是否起得來都不驗。「依 consumer lag 實際擴縮 worker」暫時沒有載具（沒有 BC 發 event；#518「已知缺口」未定案）。
`appVersion`（image.tag 空時用它）由 release.yml 的 `chart` job 在每次發版後開一支 chart-only PR 跟上（bot push 不會觸發 CI，所以它自己 `gh workflow run ci.yml`；要 repo 開「Allow GitHub Actions to create and approve pull requests」，沒開就只推 branch、summary 給 compare 連結）；chart 本身沒有 publish 到 OCI registry。

## DB 連線池容量（`postgres.max_open_conns`）

每個 process（rpc、worker、`worker prune|poison` 這些 CLI、migrate Job）自己開一個池，上限是 `postgres.max_open_conns`（預設 25）。Postgres 那頭看到的是全部加起來：

```
總連線數 ≈ Σ（各 Deployment 的 replica 數 × max_open_conns）+ 同時在跑的 CronJob / migrate Job × max_open_conns
        < max_connections − superuser_reserved_connections（預設 100 − 3）
```

預設 chart 是 rpc 2、worker 1、prune CronJob 1：

- 平常（只有 rpc + worker）：3 × 25 = 75。
- prune CronJob 在跑時：4 × 25 = **100**，還沒算 migrate Job（upgrade 時）和 rolling update 的 surge pod，已經超過 Postgres 預設 `max_connections=100` 扣掉 reserved 後的約 97 條。
- rpc HPA 開到 10、KEDA 把 worker 撐到 6：16 × 25 = 400。

這是**每個 process 都用滿上限**的最壞情況，實際不一定會到（要看負載），但**預設組態不能保證在 `max_connections=100` 的 Postgres 上最壞情況不出事**。所以上線前必須依實際 replica 數（用 `maxReplicas`）往回推，不要直接信預設值：

1. 決定 Postgres 的 `max_connections`（或前面放 pgbouncer，transaction pooling）。
2. 用「最大 replica 數」帶進公式（`rpc.autoscaling.maxReplicas`、`worker.autoscaling.maxReplicas`，不是 `replicaCount`），反推每個 pod 的 `max_open_conns`。
3. rpc 與 worker 的需求不同，分開設：`config.postgres.max_open_conns` 是共用預設，worker 用 `worker.extraEnv`（`APP_POSTGRES_MAX_OPEN_CONNS`）覆蓋。

**worker 有下限**：每個 subscriber 從領到訊息到 ack 都佔著一條交易連線（watermill-sql 在那條交易裡 `FOR UPDATE` 住 cursor），handler 的 use case 又要另一條。池子小於 `2 × subscriber 數 + 1`（例如 5 個 subscriber = 11）時，所有 subscriber 同時忙就會彼此卡死，handler 一個個撞 `worker.handler_timeout`、訊息進 poison。所以 worker 啟動時直接拒絕低於這個值的設定（`0` = 不設上限除外）。新增 BC 或 handler 時下限跟著變；目前沒有 BC 有 consumer，所以沒有 subscriber。

**rpc 沒有死鎖的下限，代價是延遲**：池子滿了請求排隊等連線（受 `http.timeout` 約束，這條沒實測）。


`max_idle_conns` 小於實際併發時，多出的連線每次用完就關、下次重連；看到 `pg_stat_activity` 的連線一直在重建就把它調到接近 `max_open_conns`。

**搭配 KEDA / HPA**：replica 數是動態的，所以公式用 `maxReplicas`。縮容時 pod 的池子隨 pod 關閉（SIGTERM 時 `NewDB` 的 cleanup 關池子）；`conn_max_idle_time`（5m）讓尖峰過後多開的連線自己回收，`conn_max_lifetime`（30m）讓 failover / DNS 變動後池子換到新的 backend。
