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
| Saga | `ordering_orders{status}`、`inventory_reservations{status}`、`*_oldest_age_seconds{status}` | ordering ↔ inventory 的漏斗：各 status 幾筆、最老一筆卡多久；PENDING 不消而 CONFIRMED / COMPLETED / CANCELLED 不動就是 saga 斷了；RESERVED 最老一筆超過 TTL 是 expire 沒跑（EXPIRED 在長 = TTL 在殺單，看是不是 TTL 太短）；CONFIRMED 一直不變 COMPLETED / CANCELLED 是業務面（沒人按 Complete），不是 bug；`$service` 選 `worker` |
| HTTP routes | `http_server_request_duration_seconds`（otelgin） | `/api/*` 的 route / status、p95 |
| Auth | `auth_decisions_total{result, method}`、`rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}` | 每秒多少 ok / public / anonymous / missing / invalid，各用什麼 credential；被拒的三種（沒帶、帶錯、role 不夠）疊在一起看。counter，`sum` |
| Identity | `identity_resolver_lookups_total{result}`、`identity_resolver_cache_entries` | key resolver 的 lookup 各 result（hit / miss / db_ok / db_miss / error）每秒多少、cache hit ratio、error 率、每個 replica 的 cache 大小。counter `sum`，cache size 是 per replica（`max by (instance)`） |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTel runtime instrumentation 的名字，OTLP 與 `/api/metrics` 相同。`/api/metrics` 用自建 registry，沒有 client_golang 的 `go_memstats_*`、`process_*` |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

Alert rules 也從 repo provision（`deployments/docker/grafana/provisioning/alerting.yaml` → folder `x-monorepo`，每分鐘評估；group
`worker-eventbus` 全部用 `max by`，gauge 是 per replica 不能 sum；group `rpc-auth` 與 `rpc-identity` 讀 counter，跨 replica `sum`）：

| rule | 條件 | 意思 / 怎麼處理 |
|---|---|---|
| Eventbus consumer lag | `eventbus_consumer_lag` > 100 持續 5m | handler 追不上或沒人在消費；看 oldest age 分辨，再加 worker replica |
| Eventbus consumer stalled | `eventbus_consumer_oldest_age_seconds` > 600 持續 5m | 該 group 完全沒前進（通常是 subscriber 連不到 DB / Redis，不是 handler 失敗——失敗的會進 poison） |
| Eventbus poison messages | `eventbus_poison_messages` > 0 持續 1m | 有訊息停在 poison 表，`task poison -- list` 看、`requeue` / `drop` 解，alert 隨 gauge 歸零 |
| Eventbus retention overdue | `eventbus_outbox_oldest_age_seconds` / `eventbus_inbox_oldest_age_seconds` > 45d 持續 1h | prune 沒在跑，或跑了刪不動；`task prune -- --older-than 720h --dry-run` 或 console Prune 頁的 dry run 會說是誰擋的。閾值設成 prune retention 的 1.5× |
| Eventbus rows held by a stale consumer group | `eventbus_outbox_oldest_held_age_seconds` > 1d 持續 30m | offsets 表裡有 group 一天沒前進；consumer stalled 沒響就是沒人跑的 group（舊 `worker.consumer_group`、E2E 的 `test-<ts>.*`），`--forget-group` / console Prune 頁的 forget 刪掉它的 cursor |
| Reservation expiry overdue | `inventory_reservations_oldest_age_seconds{status="RESERVED"}` > 36h（1.5 × 24h TTL）持續 15m | `worker expire` 沒跑或一直失敗：看 expire CronJob 的 Job，再 `task expire -- --older-than 24h --dry-run` 或 console Expire 頁 dry run 看誰到期、sweep 有沒有 ABORTED（stock 列撞版，重跑就好）。閾值跟 Helm `expire.olderThan` 綁在一起，改一個要一起改 |
| Commit short — completed order not covered | `max(delta(inventory_reservations{status="COMMIT_SHORT"}[1h]))` > 0（severity critical，不等） | 訂單已 COMPLETED（客戶被承諾有貨），但它的 reservation 先被 expire sweep 還了庫存、`OrderCompleted` 到時庫存已被別單訂走，沒扣到：處置見下方「COMMIT_SHORT 處置」。COMMIT_SHORT 是終態列、不會被清掉，所以 alert 看的是一小時內新增的數量（最後一筆之後一小時自己解除）；累積筆數看「Reservations by status」panel |
| Saga stuck — order PENDING too long | `ordering_orders_oldest_age_seconds{status="PENDING"}` > 600 持續 5m | 有訂單等 inventory 的答覆超過 10 分鐘：看 `ordering.v1.OrderCreated`（inventory 的 group）與 `inventory.v1.StockReserved`（ordering 的 group）的 lag，再看 poison 表——handler 一直 drop 的話 eventbus 幾條都不會紅，只有這條會 |
| Eventbus metrics absent | `absent(eventbus_consumer_lag)` 持續 10m | 沒有 worker 在匯出，上面幾條都是瞎的 |
| Auth credential rejections | `rate(auth_decisions_total{result="invalid"}[5m])` > 1/s 持續 5m（by `method`） | 有人在猜 key / 重放 token，或 key 換了 client 還送舊的；Loki `{service_name="rpc"} \| code="unauthenticated"` 看是哪個 procedure、request id（credential 本身不記）。rate limit 在 auth 外層，洪水先被擋 |
| Auth permission denied | `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}[5m])` > 0.1/s 持續 10m（by `rpc_method`） | 驗過身分但 role 不夠（`auth.permission_denied`）：access line 有 `sub`，是正當 operator 就補 role（`name=key:role` / JWT `roles`），不是就是在摸 admin surface |
| Identity resolver errors | `sum(rate(identity_resolver_lookups_total{result="error"}[5m])) / sum(rate(identity_resolver_lookups_total{result=~"db_ok\|db_miss\|error"}[5m]))` > 5% 持續 5m（severity critical） | resolver 查不到 DB 或 timeout：30s TTL 內驗過的 key 還能過，其他的被拒成 `auth.invalid_credential`，有 valid key 的 caller 被鎖在外面。健康時是 0，只有基礎設施故障才會有 `error`，所以門檻不用高；分母刻意只算會碰 DB 的 lookup（hit 不算）——outage 時 cache 命中照過，算進去會把比例稀釋到看不見。5m 吸收 DB failover / 連線池重建的短暫抖動。先看 Postgres，再看 Loki `{service_name="rpc"} \|= "credential resolver failed"` |

每條都帶 `__dashboardUid__` / `__panelId__`，所以「Worker — eventbus」、「Auth」與「Identity」的 panel 上會畫 alert 狀態。只有 rule 沒有送達：contact point /
notification policy 由部署環境依 `severity` label 自己接（Grafana → Alerting → Notification policies）。Alerting provisioning
只在啟動時讀，改完一樣 `task grafana-reload`。

踩雷：Grafana 13 的 file provisioner 用 inotify 監看資料夾，podman machine / Docker Desktop 的 bind mount 在 macOS 上
不會把事件傳進容器，所以改 JSON 不會自動重載 → `task grafana-reload`。另外 OTLP 的 metric export 間隔 60s，
`rate()` 剛啟動前兩分鐘是空的。`compose-down -v` 會清掉 Grafana 的 DB，但 provisioning 會重建 dashboard。
Retention 預設 72h（compose 的 `LGTM_RETENTION`，透過 image 的 `LOKI/TEMPO/PROMETHEUS_EXTRA_ARGS` 下 flag；Pyroscope v2 storage
還沒有時間型 retention）。Loki 突然沒 log 但 metrics / traces 正常，先看 `podman machine ssh df -h /`：VM 磁碟快滿時 Loki 的
WAL 會判 disk full、對每個 push 回 503 整批丟掉（`loki_ingester_wal_disk_full_failures_total`），跟 app 無關，`podman system prune` 就好。

### COMMIT_SHORT 處置

`COMMIT_SHORT` = 訂單已 COMPLETED，但 inventory 沒有賣出對應庫存：expire sweep 在 `CompleteOrder` 之後、`OrderCompleted` 被消費之前把 reservation 轉 EXPIRED 還了庫存，`OrderCompleted` 到時 `available` 已被別張單訂走（全部不動，stock 不會變負）。TTL 以小時計、窗口只有一個 consumer 跳，所以這條應該很少響；響了不是 bug，是需要人處理的缺貨。

1. 找出是哪些單：console Reservations 頁選 `COMMIT_SHORT`，或 `ListReservations status=COMMIT_SHORT`（`order_id` 對回 ordering 的訂單、line 看缺哪個 SKU 多少）。
2. 看庫存現況：`GetStock` / console Stocks 頁，`available = on_hand - reserved`。缺口 = 該單 line 的數量 − `available`。
3. 二選一處置：實體庫存其實還在（只是系統帳少扣）→ 補貨或對帳後 `SetStock` 把 `on_hand` 調到實際值，讓帳面追上已賣出的數量；實體庫存不足 → 走業務面補償（缺貨通知、退款），inventory 沒有「撤銷 COMPLETED」的路徑。
4. COMMIT_SHORT 列留著當稽核紀錄，不要刪；alert 看的是新增量，所以不必手動解除，最後一筆之後一小時自動恢復。累積筆數在「Reservations by status」panel。
5. 若頻繁出現：expire TTL（Helm `expire.olderThan`）可能比訂單從 CONFIRMED 到完成的時間還短，TTL 在殺還沒完成的單——先調長 TTL，再看 EXPIRED 筆數有沒有降下來。

並發語意（為什麼偶爾看到 `ABORTED` 或 consumer 重試）：reservation 沒有 version 欄位，`Update` 是 `WHERE status = <讀到的 status>` 的 compare-and-swap，輸的一方拿 `reservation.ErrStale`（wrap `kernel.ErrConcurrentUpdate`）；沒有 migration，轉移皆單向所以不會 ABA。
- consumer（`OrderCompleted` / `OrderCancelled`）：視為可重試的競態，eventbus 重送後依最新 status 走——例如 sweep 剛好先 expire，`OrderCompleted` 重試後走 EXPIRED 重扣（`COMMITTED` 或 `COMMIT_SHORT`）。reservation 在讀取後被改、stock 之後才載入而在 stock 層失敗（`commit_exceeds_reserved` 之類）時，use case 會重讀 reservation，status 已變就同樣回報 `ErrStale`，不會被當成終態 drop。
- `worker expire` / `ExpireReservations` RPC：一批一個交易，批內任何一筆撞版（stock version 或 reservation status）整批 rollback，CLI 下次排程重跑、RPC 回 `ABORTED` 重送即可。熱門 SKU 或剛好同時完成的訂單會讓整批重跑、延遲同批其他 reservation 的過期；持續撞版就調小 `--limit`。

## Load test（k6）

`tests/k6/` 是四個對著真的 `rpc` 打的 k6 腳本（Connect over JSON，`lib.js` 的 `rpc()` 按 method 打 tag，`API_KEY` 有設就帶 `X-API-Key`；`orders.js` / `reservation.js` 下單時誰指定 customer 見下方「Auth 組合」）：

| 腳本 | 打什麼 | 驗什麼 |
|---|---|---|
| `orders.js` | setup 先 `SetStock` 並 `UpsertProduct`（定價；CreateOrder 的價格是 catalog 的）20 個 SKU，每個 VU 迴圈 CreateOrder（1–3 行）→ GetOrder → ListOrders(customer) | `http_req_failed < 1%`、CreateOrder p95 < 300ms / p99 < 800ms、GetOrder p95 < 100ms、ListOrders p95 < 200ms、`rpc_errors == 0`（門檻是「筆電 + Docker Postgres 輕鬆過」的數字，要調是刻意調，不是失敗了改） |
| `ratelimit.js` | server 開 `http.rate_limit.rps=RPS`，以 `RPS × FACTOR`（預設 4）的 constant-arrival-rate 打 ListOrders | 被拒的每一筆都是 429 + `code: resource_exhausted` + `x-error-code: request.rate_limited` + 整數 `Retry-After ≥ 1`；放行數 ≈ `RPS × 秒數`（+ 一個 burst）、其餘全拒——limiter 是拒絕不是排隊 |
| `timeout.js` | server 開 `http.timeout=1ns`（要有 `postgres.url`：context 過期是在 DB call 才浮上來，in-memory repo 不看它） | 每一筆都是 504 + `code: deadline_exceeded` + `x-error-code: request.timeout`，不是 INTERNAL（pgx 包過的 context error 也要被對應到）；server 沒斷線 |
| `reservation.js` | 旁邊要有一個 `worker`（同一個 Postgres）：每個 VU CreateOrder（1 行、qty 1）後每 `POLL`（100ms）打一次 GetReservation，404 視為「還沒消費」 | `reservation_latency`（CreateOrder 回來 → 看到 RESERVED）p95 < `RESERVE_P95`（預設 3000ms；outbox 是 polling，空輪詢之後最多再等約一個 `worker.poll_interval`（預設 200ms）才看到新事件，筆電 10 VU 實測 p50 ≈ 0.2s / p95 ≈ 0.25s；改這項之前是 library 預設的 1s，p50 ≈ 0.9s / p95 ≈ 1.1s。腳本的 `POLL`（100ms）也佔解析度，所以數字會落在 100ms 的倍數附近）、`RESERVE_TIMEOUT`（30s）內沒等到算 `reservation_timeouts`、不是 RESERVED 算 `reservation_wrong_status`，兩者都要 0 |

```bash
task k6 -- orders                 # 對 http://127.0.0.1:8080（task run）跑 60s × 20 VU；BASE_URL / API_KEY / VUS / DURATION / RPS 用 env 蓋（auth 開著就帶 API_KEY；`orders` / `reservation` 的 setup 呼叫 `SetStock` 與 `UpsertProduct`，auth 開著時要 admin role：API_KEY 本身是 admin key 就夠，否則另帶 `ADMIN_API_KEY` 只給 setup 用）
task k6 -- ratelimit -e RPS=50    # server 要先用 APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50 起
task k6 -- reservation            # server 旁要有 bin/worker 在跑（同一個 Postgres），不然每筆都 timeout
task k6-auth-matrix               # tests/k6/auth-matrix.sh：orders / reservation 在每一種 auth 狀態各跑一次（2 VU × 5s）加上 fixtures，見下方「Auth 組合」；CI 的 k6-auth job 就是它
task k6-wire                      # tests/k6/wire.sh：四個腳本打 tests/k6/wire_server.py（回壞掉的 HTTP response），驗 key 不進 k6 的輸出 / summary / metrics tag，見下方「壞掉的 HTTP response」；不需要 Postgres 和 rpc，CI 的 k6-auth job 跑它
task k6-bench                    # tests/k6/bench.sh：效能 baseline，N≥3 次、每次一個專屬資料庫（跑完 drop），輸出 result.json / summary.md；數字與方法見 [perf-baseline.md](perf-baseline.md)；不進 CI（乾淨 runner 上手動派送 `.github/workflows/k6-bench.yml`，見該文件「在 GitHub runner 上跑」）
task k6-smoke                     # tests/k6/smoke.sh：四個腳本各自起一個 bin/rpc（:18090、local Postgres、對應的 env；reservation 排第一且多起一個 bin/worker，讓它面對空的 outbox；RUN_ID 每個腳本不同，SKU 才不會互撞），10s × 10 VU；CI 的 k6 job 就是它；CreateOrder 的 p95 門檻放寬到 CREATE_P95（預設 500ms；腳本本身預設 300）、reservation 的放寬到 RESERVE_P95（預設 5000ms；腳本 3000），shared runner 的尾巴比筆電寬
```

### Auth 組合

訂單屬於呼叫者（`docs/identity.md`）：server 認得呼叫者（key / JWT）就由它的 subject 當 customer，不認得（auth off，就算送了 key 也被忽略；或該 procedure 在 `auth.public`）就要 `customer_id`，否則 `400 order.empty_customer`。`orders.js` / `reservation.js` 不從環境（有沒有 `API_KEY`）、也不從別的 procedure（`WhoAmI`）推斷，而是由 `K6_CUSTOMER_MODE` 決定（`lib.js`）：

| `K6_CUSTOMER_MODE` | 行為 |
|---|---|
| `auto`（預設） | 每個 VU 的第一個 CreateOrder 不帶 `customer_id`：200 → 之後都不帶；`400 order.empty_customer` → 改帶腳本自己的 customer（名字含 `RUN_ID`）重送，之後都帶（沒寫入任何東西，那次 400 不算 `http_req_failed` 也不進 CreateOrder 的延遲）。**只快取這兩種答案**；其他任何回應（503、401、403…）都不快取，整個 run 立刻 abort，訊息是 `customer probe: …CreateOrder answered status <N> code <code>`，錯誤來源就是那一次真實呼叫（只印 RPC / status / Connect code，不印 response body 或其他 header：上游或 proxy 可能把 `X-API-Key` 反射進去，code 只印已知值（16 個 Connect code 和平台的 x-error-code，見 `tests/k6/codes.js`），其他一律 `<unrecognized>`；輸出前再把這次用的 `API_KEY` 值換成 `<redacted>`）。`rpc()` 不跟 redirect（`redirects: 0`：3xx 就是一個要回報的錯誤，Location 不印），`name` tag 固定為 RPC 名，四支腳本的 `systemTags` 不含 `url`（和 `error`，見下方「壞掉的 HTTP response」）——合法 HTTP 的反射進不了 k6 自己的 warning 和 metrics；`task k6-auth-matrix` 的反射 cell 同時檢查 log、`--summary-export` 和 `--out json`。ListOrders 跟 CreateOrder 解出來的結果走（auth off 篩腳本的 customer、有 principal 看自己的，admin 看全部） |
| `include` / `omit` | 不探測，一律帶 / 一律不帶 `customer_id`（探測的那一次 CreateOrder 有顧慮、或要固定行為時用） |

`task k6-auth-matrix` 驗證的組合（`tests/k6/auth-matrix.sh`，真的 rpc + Postgres，reservation 旁有 worker，各自的 `RUN_ID` / SKU / customer；腳本原本的 thresholds 不動，另外要求 `rpc_ok`（成功的 CreateOrder / GetOrder / ListOrders，reservation 是 CreateOrder / CompleteOrder / GetReservation）都 > 0）：

| auth | key | orders / reservation |
|---|---|---|
| on（`required`） | role-less key（setup 的 SetStock / UpsertProduct 用另帶的 `ADMIN_API_KEY`） | 支援（`auto` → 不帶 customer，只能下自己的單） |
| on | admin key | 支援（`auto` → 不帶 customer；ListOrders 會列全部） |
| off | 不帶 | 支援（`auto` → 帶腳本的 customer） |
| off | 帶 key（server 忽略） | 支援（同上，不看 `API_KEY`） |
| on，只有 `WhoAmI` 在 `auth.public`，role-less key | | 支援（WhoAmI 不被問，CreateOrder 的答案才算數） |
| on，只有 `CreateOrder` 在 `auth.public`，role-less key | | **不支援**：CreateOrder 是 anonymous（要 customer），但帶 key 的 GetOrder / ListOrders 看不到別人的單 → `order.not_found` / `order.permission_denied`。matrix 要求它失敗、輸出看得到 `order.not_found` |
| `WhoAmI` 回一次 503（透過 `tests/k6/fault_proxy.py`，test-only，不改產品行為） | | 不受影響（不依賴 WhoAmI） |
| 判斷用的第一個 CreateOrder 回一次 503 | | abort，訊息含 `customer probe` 和 503 / `unavailable`，不會被當成 anonymous 快取 |

### 壞掉的 HTTP response（`task k6-wire`）

上游或 proxy 回的不是合法 HTTP（status code 是 key、version 是 key、header 行沒有冒號且帶著 key）時，Go 的 parser 把「讀不懂的那段」原樣放進錯誤，而 k6 在 `http.post` 回傳之前就把它印成 `Request Failed` warning，並寫進每一筆樣本的 `error` tag——`lib.js` 的 `redact` 只包得到腳本自己印的診斷，在這之前攔不到。實測（k6 v2.2.0）：只在腳本裡包 try/catch **沒有用**（請求失敗時 `http.post` 不丟例外，warning 已經印了）。做法是三件事一起：

1. `options.throw: true`（`lib.js` 的 `throwOnError`）：請求失敗改成丟例外，k6 不再自己印 warning；
2. `rpc()` 抓住例外，只用訊息挑一個固定分類，訊息本身不印、不放進回傳：回一個 `status: 0` 的 response，`errorCode()` / `knownCode()` 給 `transport.<class>`——`malformed_response`（status / version / header / chunk 讀不懂）、`truncated_response`（`unexpected EOF`）、`timeout`、`connection_refused`、`dns`、`connection_closed`（對方直接關線）、`other`；`rpc()` 每個 VU 前三次印 `<RPC>: no response, transport.<class>`（整句過 `redact()`：決策用 `knownCode()`，輸出和 `errorCode()` 一樣遮罩，key 剛好等於 `transport.<class>` 也不會出現；`wire.sh` 有 14 個這樣的 cell）。這次不是 `rpc_duration` 的樣本；
3. `systemTags` 拿掉 `error`（自由文字，就是 parser 的訊息），保留 `error_code`（k6 的固定數字分類，例如 1000 = 一般 transport 錯誤）。

`rpc()` 以外的任何 `http.*` 呼叫都不受保護：沒被抓到的例外 k6 會連訊息一起印。新腳本一律走 `rpc()`，`options` 帶 `throw: throwOnError`。

對監控查詢的影響：repo 裡沒有東西讀 k6 的 `error` tag（k6 沒接 Grafana / Prometheus remote write；`kind/scale.sh` 只看 exit code 和 log），所以沒有 dashboard 或 alert 受影響。自己用 `--out` 接 k6 metrics 的人，原本 `error` tag 現在沒有了，改看 `error_code`（或 `status == 0`）；細節看 log 的 `transport.<class>`。

另外 transport 失敗不再被默默放過：k6 對沒有 response 的請求不計 `http_req_failed`，所以 `timeout.js` 打到一個回壞掉 HTTP 的 server 時，原本每筆都在 `check` 丟例外、`timed_out` 沒有樣本 → `rate==1` 空過、exit 0；現在 `check` 看得到 `status 0`，`timed_out` 是 0% → exit 99。

`tests/k6/wire.sh`（`task k6-wire`，不需要 Postgres 和 rpc，約 1 分鐘）：四支腳本 × 六種壞掉的回應（`tests/k6/wire_server.py`：`status` / `version` / `header` / `chunk` 帶著請求的 `X-API-Key`，`truncated` body 不完整，`closed` 連線直接關），每個 run 都要非零退出，`API_KEY`（自造的假 key）不能出現在 k6 輸出、`--summary-export`、`--out json` 的任何一行，metrics 不能有自由文字的 `error` tag，`closed`（key 根本沒上線）要有 `transport.<class>` 的診斷。每個 cell 還要證明 fixture 真的在場：server 要在 5 秒內 listen、跑完仍活著、自己印出 `served <mode>`（壞回應確實送出），而且 k6 回報的失敗分類必須恰好是該 mode 的（`status` / `version` / `header` / `chunk` → `malformed_response`、`truncated` → `truncated_response`、`closed` → `connection_closed`），出現其他分類（例如 server 缺席時的 `connection_refused`）就紅——光看 exit 非零和沒有 key 證明不了什麼。最後兩個負例 cell 讓 `wire.sh` 自己再跑一次：server 不啟動（`WIRE_SERVER_CMD=false`）、server 回錯的 mode（`WIRE_SERVE_AS=closed`），它必須失敗，否則 gate 本身標紅。失敗用 go test 的 `--- FAIL:` 格式印，`scripts/red-stage.sh` 讀得懂。

### 合法 JSON 的 status 欄位反射（`task k6-wire`）

HTTP 和 JSON 都合法、但 enum 欄位被上游或 proxy 填成請求的 `X-API-Key`（`reservation.status`、`order.status`），`reservation.js` 原本把 raw 值 `String(status)` 直接放進 `reservation_wrong_status` / `order_not_confirmed` / `reservation_not_committed` 的 `status` tag，key 會進 `--out json`（#243）。做法：

- tag 只走 `lib.js` 的 `statusTag(status, known)`：值在 `codes.js` 的 `ORDER_STATUSES` / `RESERVATION_STATUSES`（proto enum）裡才原樣輸出，其他一律 `<unrecognized>`，再過 `redact()`（key 剛好等於合法 enum 值時變 `<redacted>`），和 `errorCode()` 同一套；
- 判定不動：`if` 比的是 raw 值，錯的 status 照樣累加 counter、threshold 照樣紅，遮罩只影響輸出；
- 新增 enum 值要同步 `codes.js`，否則 tag 先顯示 `<unrecognized>`；
- 其他腳本沒有從 response 取值放 tag（`orders.js` / `ratelimit.js` / `timeout.js` 只用固定字串，`console.error` 只印 `res.status` 數字和 `errorCode()`）。

`wire.sh` 在跑 `reservation` 時多四個 `wire-reservation-reflect-*` cell（`tests/k6/status_server.py`，合法 200 + JSON，不需要 Postgres）：三個 counter 各一個（status 是 key → `<unrecognized>`），加一個 key 設成合法 enum 值 `RESERVATION_STATUS_EXPIRED`（→ `<redacted>`）；每個 cell 要求 k6 非零退出、counter 真的有樣本且 tag 是固定值，key 不在 log、`--summary-export`、`--out json`。

CI 的 `k6-auth` job 和 `k6` job 平行；matrix 的 cell 名字可以接在 task 後面只跑幾個：`task k6-auth-matrix -- on-admin-orders whoami-public`。其他沒有列的組合（JWT、`optional` 模式、其他 procedure 的 public）沒有驗證，不保證。

worker 有跑時每張訂單都會讓 inventory 去 reserve，`orders.js` 不等它——要看 consumer 追不追得上就看 `eventbus_consumer_lag`；這也是 Helm `worker.autoscaling.metric: lag` 的壓力源。`reservation.js` 才等：它量的是單筆訂單從下單到 RESERVED 的端到端延遲，是 worker 鏈的 benchmark 基準。

## Worker 輪詢間隔（`worker.poll_interval`）

`postgres` broker 的 handler 靠輪詢 outbox 表拿事件（watermill-sql 的 `PollInterval`；redis 模式只有 forwarder 的 SQL 端輪詢）。watermill-sql 只有在一次查詢**沒拿到訊息**之後才等滿一個間隔，所以這個間隔是閒置時的**上限**、不是下限：新事件在等待期間任何時刻到達，額外延遲落在 0 到約一個間隔之間；找到訊息時 subscriber 不會等，下一次查詢立刻發出，所以忙碌時看到的是 handler 本身的速度。排程、DB 與 handler 處理仍會再加上延遲。實測：保留 library 預設 1s、空 poll 後 900ms 才 publish，事件約 104ms 就送達（watermill-sql v4.1.5 `pkg/sql/backoff_manager.go`：只有 noMsg 才回傳 `pollInterval`）。

| 間隔 | `reservation.js`（10 VU、10s，同一台筆電） | 閒置時 worker 對 Postgres 的負載（5 個輪詢迴圈） |
|---|---|---|
| 1s（舊預設） | p50 0.82–0.93s / p95 1.03–1.12s | ≈ 6 xact/s |
| 250ms | p50 0.21s / p95 0.32s | — |
| **200ms（新預設）** | p50 0.20s / p95 0.21–0.31s | ≈ 28 xact/s（≈ 73 statements/s） |
| 100ms | p50 0.10s / p95 0.11s | ≈ 54 xact/s |

**成本怎麼算**：每個輪詢迴圈每個間隔一次輪詢（一個短交易，約 3 條 statement：offsets `FOR UPDATE`、取訊息、更新 cursor），所以

```
輪詢 xact/s ≈ 輪詢迴圈數 × replica 數 ÷ 間隔(秒)
輪詢迴圈數 = postgres broker：所有 BC 的 handler subscription 數
           = redis 模式：forwarder 對每個 distinct forwarded topic 各 Subscribe 一次（apps/worker/app.go），一個 topic 一個迴圈
```

本機這組 stack 是 5 個輪詢迴圈（postgres：5 個 handler subscription；redis：5 個 distinct topic，不是「handler 數 + 1」）：200ms、2 個 replica ≈ 50 xact/s；KEDA 把 worker 撐到 10 個 replica 就是 250 xact/s（1s 時是 50）。這些輪詢是索引上的小查詢，但它們佔連線、也佔 `max_connections`——調小間隔前先算好 replica 上限。

**要不要調**：看 `eventbus_consumer_lag` 與 `eventbus_consumer_oldest_age_seconds`。
- 平常 lag 為 0、`oldest_age` 幾乎都在一個間隔之內：剩下的主要延遲就是輪詢等待（最多約一個間隔），要更低才縮間隔（如 100ms）。
- lag 持續 > 0 且 `oldest_age` 遠大於間隔：瓶頸是 handler 吞吐，不是輪詢；縮間隔沒用，該加 replica（Helm `worker.autoscaling`）或看 handler。
- Postgres 的 `pg_stat_database.xact_commit` 在閒置時就很高、或連線數吃緊：放大間隔（如 500ms–1s），延遲換 DB 負載。
重送（nack）與查詢失敗後的等待仍是 library 的 1s：handler 失敗由 `Register` 的 Retry 在行程內重試，不靠 nack；DB 出錯時也不該更頻繁地打它。

設定：`worker.poll_interval` / `APP_WORKER_POLL_INTERVAL`（Helm `config.worker.poll_interval`），必須 > 0。

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
| expire CronJob | `worker expire --older-than <expire.olderThan>`（預設 24h、每 15 分鐘、`Forbid`）：reservation TTL，RESERVED 超過就還庫存 + 請 ordering 取消。alert「expiry overdue」的 36h 是 24h 的 1.5×，改一個要一起改；dry run / 小量跑走 `InventoryService.ExpireReservations`（console Expire 頁，要 admin role） |
| identity bootstrap（不在 chart 裡） | 第一個 account + API key：在**操作者本機**跑一次 `worker bootstrap`，經 `kubectl port-forward` 連 release 的 DB，secret 只印在你自己的 terminal 一次（做法見下面「identity bootstrap」）；**不要在叢集裡跑**——Pod 的 stdout 會進 Pod log、也可能被 log collector 收走。config `auth.api_keys` 留作 break-glass。已有 account 就是 no-op，但不要同時跑兩個 |
| HPA / PDB / Ingress | rpc：CPU HPA（開了就不 render `replicas`、要有 `resources.requests.cpu`）、PDB 只在可能 >1 replica 時 render、Ingress 走 HTTP/1.1（Connect / gRPC-Web），原生 gRPC 要另外 h2c |
| console Deployment（`console.enabled`，預設 off） | `web/apps/console` 的 nginx image：唯一的 env 是 `RPC_UPSTREAM=http://<release>-rpc:<rpc.service.port>`（chart 算出來），**沒有** `APP_CONFIG`、沒有 Secret——nginx 看不到 Postgres URL 和 API key，auth 開著時使用者在 console 的 Credential 對話框貼自己的 key / JWT；probe 打 nginx 自己的 `/healthz`（rpc 的健康是 rpc probe 的事）；同一組 `podSecurityContext`（uid 65532、read-only rootfs）+ `/tmp` emptyDir 就夠。自己的 Service / Ingress（`console.ingress`，純 HTTP/1.1） |
| ServiceMonitor（`rpc.serviceMonitor.enabled`，預設 off） | Prometheus Operator 抓 rpc Service 的 `http` port、path `/api/metrics`（gin route，auth 開著也不擋）；operator 靠 label 選 ServiceMonitor，`serviceMonitor.labels` 放它要的（kube-prometheus-stack 是 `release: <name>`）。只管 rpc——worker 沒 HTTP server，它的 metrics 只能走 `config.otel.endpoint` |
| NetworkPolicy（`networkPolicy.enabled`，預設 off） | 每個 Deployment 一張、只管 ingress（egress 不碰：DB / Redis / collector 在哪是 cluster 的事）。rpc 永遠放行同 release 的 console pod，加上 `networkPolicy.rpc.ingressFrom`（NetworkPolicyPeer list；**空 = 該 port 對任何來源開**，要收緊就列 ingress controller 與 Prometheus 的 namespace）；worker 不收任何 ingress；console 同一套 `console.ingressFrom`。要 CNI 真的實作 NetworkPolicy 才有效，kind 預設的 kindnet 不會，所以 helm-kind 驗不了，check.py 驗形狀 |
| worker autoscaling | replica 共用 `consumer_group` 就是分工，但 handler 是 DB-bound，CPU 低時 lag 照樣長，所以 `worker.autoscaling.metric` 預設 `lag`：KEDA `ScaledObject` 用 Prometheus trigger 查 `max(eventbus_consumer_lag{consumer_group=~"<group>\\..*"})`（gauge 是 per replica，取 max 不 sum；要 `config.otel.endpoint`，否則拒絕 render）；沒 KEDA 的 cluster 用 `metric: cpu` 出普通 HPA。實測踩到的三件事都做成 render 時就擋：`lag.prometheusAddress` 要 `http://<svc>.<ns>.svc:<port>`（KEDA operator 從**它自己的** namespace 解析，裸 Service 名不通）；`minReplicas` 必須 ≥ 1（lag gauge 是 worker 自己報的，0 個 replica 就沒人報）；`pollingInterval` / `cooldownPeriod` 只對 scale-to-zero 有效，縮回去的節奏是 HPA 的 `behavior.scaleDown`（預設 stabilization 300s，`worker.autoscaling.behavior` 透傳） |

**migration 的鎖（ordering `0008`，`CreateOrder` 冪等）**：`0008_order_idempotency` 對 `ordering.orders` 加兩個 nullable 欄位（不重寫表、不回填），再建 partial unique index（`CREATE UNIQUE INDEX`，不是 `CONCURRENTLY`——migration 在**一個交易**裡跑）。`ALTER TABLE` 取 **ACCESS EXCLUSIVE lock 並持有到整個 migration commit**，建 index 的整表掃描也在這把鎖底下：`ordering.orders` 的**讀與寫**（GetOrder / ListOrders / CreateOrder / Cancel / Complete / Confirm）都會等到整個 migration 結束，不只是寫入；讀流量照進來會逾時。表小（本機、staging）是毫秒級，可忽略；**大表請排維護窗口，且窗口要涵蓋讀流量**（先把 rpc / worker `kubectl scale` 到 0 或擋掉流量，再讓 migrate hook 跑）。順序：先 migrate 再升 rpc（chart 的 `pre-upgrade` hook 已保證）；舊 rpc 不認識新欄位也不寫它們，新 rpc 對 NULL 的列照舊，所以 rolling upgrade 期間兩種版本可並存。回滾：`migrate down` 一步會丟掉 key 與 hash（已記住的冪等保護隨之消失，訂單不受影響）；`down` 同樣在一個交易裡取 ACCESS EXCLUSIVE。

**identity bootstrap（在操作者本機跑，不要在叢集裡跑）**：secret 是 `worker bootstrap` 的 stdout，叢集裡任何 Pod（Job、`kubectl run`、`kubectl exec`）的 stdout 都會進 Pod log，也可能被 log collector 收走——所以只在你自己的 terminal 跑，經 port-forward 連 DB。chart 本身沒有 Postgres（`secrets.postgresUrl` 指到哪就是哪；kind 的 `deps.yaml` 才有 Service `postgres`），`<fullname>` 是 chart 的 fullname（release `api` 就是 `api-x-monorepo`），`<release-ns>` 是 release 裝的 namespace（跟 `<db-ns>` 不一定同一個）：

```bash
# 1. DB 的位置在 release 的 Secret 裡：APP_POSTGRES_URL（用 secrets.existingSecret 的話換成那個名字）。
#    host 若是 <svc>.<ns>.svc，就把那個 Service port-forward 到本機；managed DB 就走你們原本連它的 proxy / bastion。
kubectl -n <db-ns> port-forward svc/<db-svc> 15432:5432 &   # 開著，做完 kill 它

# 2. 取 URL、改寫、跑 bootstrap，全部在同一個 fail-fast 子 shell 裡：任何一步失敗就中止、不會跑到 bootstrap。
#    原始 URL 與改寫後的 URL 只存在這個子 shell 的變數裡（不 export、不 echo、不進 history）；最終的 URL 會以 APP_POSTGRES_URL 傳給 go run 這個子程序。
#    shell 只做改寫；連線安全由 bootstrap 檢查：--expect-target 必填（沒帶就拒絕）、每個 attempt 只准連它、TLS 要驗證，不通過就在連線前拒絕。
(
  set +x                          # 外層若開了 xtrace（set -x / bash -x / zsh -x），先關掉，不然下面每一行連同密碼都會被印出
  set -euo pipefail
  port=15432                      # 與上面 port-forward 的本機 port 一致
  ca=/path/to/db-ca.pem           # DB 的 CA
  raw=$(kubectl -n <release-ns> get secret <fullname>-env -o jsonpath='{.data.APP_POSTGRES_URL}' | base64 -d)
  [ -n "$raw" ] || { echo "abort: APP_POSTGRES_URL is empty or missing in the Secret" >&2; exit 1; }
  # 改寫之前先看原始 URL 的 authority（:// 之後到第一個 / 或 ? 之前）：@ 必須剛好一個。密碼裡的 @ 要編碼成 %40；
  # 未編碼的 @ 讓下面的 sed 把一段密碼當成 host 截掉、改寫成「合法但密碼不對」的 URL，所以直接中止（不印 URL，也不會呼叫 go）。
  rest=${raw#*://}; auth=${rest%%[/?]*}; ats=${auth//[^@]/}
  [ "${#ats}" -eq 1 ] || { echo "abort: the URL's authority must contain exactly one @ (encode an @ in the password as %40)" >&2; exit 1; }
  # 只做兩件事，而且都不去「讀」URL 的內容：
  #  (a) host:port 換成轉發的本機 port：只動第一個 @ 到下一個 / ? 或結尾那一段（合法 URL 的 userinfo 裡 @ 一定是 %40，所以這段就是 authority），密碼原樣不動；
  #  (b) 在尾端附加 sslmode=verify-ca 與 sslrootcert（port-forward 之後 hostname 對不上，所以用 verify-ca 驗憑證鏈）。不搜尋、不取代原本的 sslmode：
  #      原本若有 sslmode，pgx 對重複的 key 以最後一個為準（pgconn/parse_url.go: settings[key] = value），附加的會贏。
  url=$(printf '%s' "$raw" | sed -E "s#@[^/?]+([/?]|\$)#@127.0.0.1:${port}\1#")
  case "$url" in *\?*) url="${url}&sslmode=verify-ca&sslrootcert=${ca}" ;; *) url="${url}?sslmode=verify-ca&sslrootcert=${ca}" ;; esac
  # 用空的 config 檔跑：不會讀 configs/local.yaml，萬一 env 沒生效也只會報 postgres.url is required，不會連到本機 DB。
  # 不用 `task bootstrap`：它會把 APP_CONFIG 設成 configs/local.yaml、還帶 --insecure-allow-plaintext（只給本機開發 DB）。release 同版 code 在 repo 根目錄跑，或換成同版 worker binary（./worker bootstrap）。
  cfg=$(mktemp); trap 'rm -f "$cfg"' EXIT
  APP_CONFIG="$cfg" APP_POSTGRES_URL="$url" go run ./apps/worker bootstrap --expect-target "127.0.0.1:${port}" --name root --role <release 的 auth.admin_role> --key-name bootstrap
)
```

- DB 要開 TLS 才走得通這份 runbook：chart 沒有 Postgres，kind 的 `deps.yaml` 那個也沒開 TLS。自備的 Postgres 開 `ssl=on`（自簽 CA 也行），`ca` 指到簽它的 CA；`verify-ca` 不比對主機名，所以 server 憑證的 SAN 不需要含 `127.0.0.1`。app 自己的 `secrets.postgresUrl` 可以維持 `sslmode=require`（只有 bootstrap 這條路要驗憑證）。
- 空 config 代表 `--role` 的預設是內建預設值、不是 release 的 `auth.admin_role`；兩者不同時要明寫 `--role`，不然 key 會沒有管理權限。
- TLS 一律驗憑證。透過 port-forward 連 `127.0.0.1` 時 hostname 對不上，所以用 `verify-ca` + `sslrootcert`（憑證鏈照驗，只是不比對主機名；上面的指令在 URL 尾端附加這兩個）。或者改從能以原主機名連到 DB 的地方（bastion、managed DB 的 proxy）跑，保留 `verify-full`，這時不需要 port-forward 與 `sed`，`--expect-target` 填那個 host:port。
- 附加（而不是取代）的理由：整串 URL 做取代會改到不該改的字，例如密碼 `pw_sslmode=verify-full` 或 `application_name=sslmode=verify-full`。附加只依賴「重複 key 以最後一個為準」；這個語意就算哪天變了，bootstrap 的檢查是看 pgx 最終解析出的結果，TLS 沒驗證就在連線前拒絕——會安全地失敗，不會悄悄用錯的設定連線。密碼要用 `%40`、`%2F`、`%3F` 編碼 `@`、`/`、`?`；未編碼的 `@` 是 malformed URL：上面的指令在改寫前就會中止（authority 裡 `@` 不是剛好一個），不會呼叫 `go`。
- `sslmode=require` 配有效的 `sslrootcert`，pgx 視同 `verify-ca`（跟 libpq 一致），檢查會放行；沒有 `sslrootcert` 的 `require`（只加密、不驗證 server）、`disable`、`allow`、`prefer` 一律拒絕。
- 檢查在 bootstrap 裡做，不在 shell：它用 `pgx.ParseConfig`（gorm 開連線時用的同一個 parser）解析 `postgres.url`，連線前要求主 attempt 與每個 fallback 都驗證 server（`verify-ca` 帶 `sslrootcert`，或 `verify-full`；`disable` / `allow` / `prefer` / 沒有 `sslrootcert` 的 `require` 都拒絕），並要求每個 attempt 的 host:port 都**剛好等於 `--expect-target`**（擋 query 裡的 `host=` 或多個 host）。**`--expect-target` 是必填**，沒帶或帶空值，在讀 config、解析 URL 之前就以「--expect-target is required」拒絕：密碼含未編碼的 `@` 時，pgx 解析出的 host 可能是密碼的一段，而且可能剛好長得像合法的 host 名稱（`postgres://u:pw@SECRETLABEL/app` 的 `SECRETLABEL`），單看 URL 分不出來；resolver 的錯誤（`lookup <那一段>`）會被 main 寫進 log。所以只有操作者自己指定的目標可信，其他一律不信。結構檢查（host 必須是 IP、RFC 1123 DNS 名稱——可以有結尾一個根點，如 `localhost.`——或 unix socket 目錄，否則以「postgres.url has an invalid host」拒絕）只是第二道防線：它擋得住 `SECRET@127.0.0.1` 這種不成形的 host，擋不住像 host 的密碼片段，真正擋住的是 target 釘選。所以 URL 怎麼寫（空白 key、重複 key、`%` 編碼）都以 pgx 實際解析的結果為準。拒絕訊息只有原因（target 不符時加上你自己帶的 `--expect-target` 值），**不含任何從 URL 或 `PG*` env 解析出來的值**（host、port、user、dbname、密碼）：密碼含未編碼的 `@` 時，pgx 會把密碼的一段當成 host，印出來就是把密碼寫進 log。
- 殘餘風險（TOCTOU）：檢查和實際連線各解析一次 `postgres.url`。前提是操作期間，本機的 CA 檔（`sslrootcert`）、service file、`PG*` env 由你控制且不變；有人能在這兩步之間換掉它們，檢查就保證不了連線。
- `--insecure-allow-plaintext` 只跳過 TLS 檢查，`--expect-target` 照樣必填、照樣釘選。它只給本機開發用的 DB——`task bootstrap` 帶著它，並把 target 釘在 compose 的 Postgres（`127.0.0.1:${POSTGRES_PORT:-5432}`，預設 `127.0.0.1:5432`；`configs/local.yaml` 的 `postgres.url` 改了 port 就設同一個 `POSTGRES_PORT`）。**不要用它連 release 的 DB。**
- 第一次跑印出 `X-API-Key: xk_…`（只這一次，庫裡只有 hash），抄到密碼管理器；再跑一次是 `already bootstrapped`、什麼都不寫。做完停掉 port-forward。port-forward 在有些環境（kind + podman 實測）會在 client 連線被重置後自己結束（`lost connection to pod`），重跑時若出現 `connection refused`，重開 port-forward 再跑。
- 這把 key 要 `config.auth.mode` 是 `optional` 或 `required` 才有作用（chart 預設 `off`，不檢查 credential）。`required` 時不用設 `secrets.apiKeys`：DB 裡的 key 由 resolver 驗（kind 實測：沒帶 key 回 401、帶 bootstrap 出來的 key 呼叫 `IdentityService/WhoAmI` 回 `root` / `admin`）。
- 已有 account 就是 no-op，但**不要同時跑兩個**：guard 是 read 不是 constraint，兩個 run 在空 store 上搶跑會各建一個。

`worker bootstrap` 因為輸出失敗（terminal 被關、pipe 斷掉）而 exit 非 0，代表 account 與 key **已經建立**、secret **已經遺失**（庫裡只有 hash，重跑是 no-op、不會重印）。補救：用 config `auth.api_keys` 的 break-glass key 登入，再呼叫 `CreateApiKey` 開一把新 key（錯誤訊息裡有沒送達那把的 key ID 與 prefix，要的話 `RevokeApiKey` 它）。

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
SetStock → UpsertProduct → CreateOrder → worker 透過 Redis Streams 消費 OrderCreated → GetStock 看到 reserved；console 的 `/reservations` 回 `index.html`、`/api/readyz` 經 nginx proxy 到 rpc。本機約 1 分鐘（cluster 已存在時），`task helm-kind-down` 刪 cluster。
另一條 `task helm-kind-keda`（CI 同一個 job 的另一個 matrix leg，和 `helm-kind` 平行、各自一個 cluster；兩者共用的底座是 `task helm-kind-up`：cluster、image、Postgres / Redis）：裝 KEDA、`kind/monitoring.yaml` 放一個 OTLP collector（gRPC :4317 → Prometheus exporter :8889）
和一個 5s scrape 的 Prometheus，用 `kind/keda-values.yaml` 重裝 chart（`OTEL_METRIC_EXPORT_INTERVAL=5000`、threshold 50、scaleDown stabilization 30s），
`kind/scale.sh` 用 `tests/k6/orders.js` 灌 45 秒訂單：實測 lag 0 → 2000+，worker 13 秒內 1 → 4 replica，backlog 消化完縮回 1。
`appVersion`（image.tag 空時用它）由 release.yml 的 `chart` job 在每次發版後開一支 chart-only PR 跟上（bot push 不會觸發 CI，所以它自己 `gh workflow run ci.yml`；要 repo 開「Allow GitHub Actions to create and approve pull requests」，沒開就只推 branch、summary 給 compare 連結）；chart 本身沒有 publish 到 OCI registry。

## DB 連線池容量（`postgres.max_open_conns`）

每個 process（rpc、worker、`worker expire|prune|poison` 這些 CLI、migrate Job）自己開一個池，上限是 `postgres.max_open_conns`（預設 25）。Postgres 那頭看到的是全部加起來：

```
總連線數 ≈ Σ（各 Deployment 的 replica 數 × max_open_conns）+ 同時在跑的 CronJob / migrate Job × max_open_conns
        < max_connections − superuser_reserved_connections（預設 100 − 3）
```

預設 chart 是 rpc 2、worker 1、expire 與 prune 兩個 CronJob 各 1：

- 平常（只有 rpc + worker）：3 × 25 = 75。
- 兩個 CronJob 同時在跑：5 × 25 = **125**，還沒算 migrate Job（upgrade 時）和 rolling update 的 surge pod。即使只有一個 CronJob 在跑，也是 4 × 25 = 100，已經超過 Postgres 預設 `max_connections=100` 扣掉 reserved 後的約 97 條。
- rpc HPA 開到 10、KEDA 把 worker 撐到 6：16 × 25 = 400。

這是**每個 process 都用滿上限**的最壞情況，實際不一定會到（要看負載；`orders.js` 100 VU 才把單一 rpc 推到 25），但**預設組態不能保證在 `max_connections=100` 的 Postgres 上最壞情況不出事**。所以上線前必須依實際 replica 數（用 `maxReplicas`）往回推，不要直接信預設值：

1. 決定 Postgres 的 `max_connections`（或前面放 pgbouncer，transaction pooling）。
2. 用「最大 replica 數」帶進公式（`rpc.autoscaling.maxReplicas`、`worker.autoscaling.maxReplicas`，不是 `replicaCount`），反推每個 pod 的 `max_open_conns`。
3. rpc 與 worker 的需求不同，分開設：`config.postgres.max_open_conns` 是共用預設，worker 用 `worker.extraEnv`（`APP_POSTGRES_MAX_OPEN_CONNS`）覆蓋。

**worker 有下限**：每個 subscriber 從領到訊息到 ack 都佔著一條交易連線（watermill-sql 在那條交易裡 `FOR UPDATE` 住 cursor），handler 的 use case 又要另一條。池子小於 `2 × subscriber 數 + 1`（目前 5 個 subscriber = 11）時，所有 subscriber 同時忙就會彼此卡死，handler 一個個撞 `worker.handler_timeout`、訊息進 poison。實測 `max_open_conns=1`：reservation.js 每筆都 10s 後 `handler timed out`。所以 worker 啟動時直接拒絕低於這個值的設定（`0` = 不設上限除外）。新增 BC 或 handler 時下限跟著變。

**rpc 沒有死鎖的下限，代價是延遲**：池子滿了請求排隊等連線（受 `http.timeout` 約束，這條沒實測）。本機 100 VU 打 `orders.js`（10s）：

| `max_open_conns` | server 端看到的連線數（尖峰） | 吞吐 |
|---|---|---|
| 0（不設上限） | 35 | 2745 req/s |
| 25（預設） | 25 | 2820 req/s |
| 10 | 11（多 1 條的原因沒查） | 2793 req/s |

上限住了，吞吐沒掉（這台筆電上；100 VU 時連線數不是瓶頸）。`max_idle_conns` 小於實際併發時，多出的連線每次用完就關、下次重連；看到 `pg_stat_activity` 的連線一直在重建就把它調到接近 `max_open_conns`。

**搭配 KEDA / HPA**：replica 數是動態的，所以公式用 `maxReplicas`。縮容時 pod 的池子隨 pod 關閉（SIGTERM 時 `NewDB` 的 cleanup 關池子）；`conn_max_idle_time`（5m）讓尖峰過後多開的連線自己回收，`conn_max_lifetime`（30m）讓 failover / DNS 變動後池子換到新的 backend。
