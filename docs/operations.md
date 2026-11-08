# Operations：Compose、k6、Helm

<sub>← [README](../README.md)</sub>

## Local infra（Compose）

```bash
task compose-up      # podman compose（沒有 podman 就 docker compose）起 Postgres（:5432，user/pass/db 都是 app）＋ otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up   # 自動 seed 的 configs/local.yaml 已把 postgres.url 指向它，不用 export
task run             # app 在本機跑，不進容器
task worker          # 事件 consumer process（另開一個 terminal）
task payment-worker  # payment 的事件 consumer（#410；`ordering.require_payment: true` 時要和 `task payment-rpc` 一起跑，讀 configs/payment-rpc.yaml）
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
| Payment | `payment_reconcile_eligible`、`payment_reconcile_oldest_unchecked_seconds`、`payment_alerts_total{alert}`、`ordering_payment_event_total{event,effect}`（worker 把付款事件套到訂單的結果；`unexpected` 應永遠是 0），加上 payment 自己 consumer group（`payment-rpc.payment`）的 `eventbus_consumer_lag`／`eventbus_consumer_oldest_age_seconds` 與 `eventbus_poison_messages` | 補查的待查筆數與最久沒被問過的等待時間（`expire` CronJob 每輪寫一次，read 用 `last_over_time`）、需要人看的 payment 告警各類別、payment consume 的積壓；reconcile gauge 沒資料 = 沒有量測，不是沒有積壓。標籤用 `job="payment-rpc"`，不跟 `$service`。chart 怎麼部署見下面「chart 怎麼部署 payment-rpc」 |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTel runtime instrumentation 的名字，OTLP 與 `/api/metrics` 相同。`/api/metrics` 用自建 registry，沒有 client_golang 的 `go_memstats_*`、`process_*` |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

Alert rules 也從 repo provision（`deployments/docker/grafana/provisioning/alerting.yaml` → folder `x-monorepo`，每分鐘評估；group
`worker-eventbus` 全部用 `max by`，gauge 是 per replica 不能 sum；group `payment` 讀 `payment_*`（counter `sum`、reconcile gauge 用 `last_over_time`）；group `rpc-auth` 與 `rpc-identity` 讀 counter，跨 replica `sum`）：

| rule | 條件 | 意思 / 怎麼處理 |
|---|---|---|
| Eventbus consumer lag | `eventbus_consumer_lag` > 100 持續 5m | handler 追不上或沒人在消費；看 oldest age 分辨，處置見「Worker 輪詢間隔」的「要不要調」（加 replica 是否有效取決於 broker，那裡有量測） |
| Eventbus consumer stalled | `eventbus_consumer_oldest_age_seconds` > 600 持續 5m | 該 group 完全沒前進（通常是 subscriber 連不到 DB / Redis，不是 handler 失敗——失敗的會進 poison） |
| Eventbus poison messages | `eventbus_poison_messages` > 0 持續 1m | 有訊息停在 poison 表，`task poison -- list` 看、`requeue` / `drop` 解，alert 隨 gauge 歸零 |
| Eventbus retention overdue | `eventbus_outbox_oldest_age_seconds` / `eventbus_inbox_oldest_age_seconds` > 45d 持續 1h | prune 沒在跑，或跑了刪不動；`task prune -- --older-than 720h --dry-run` 或 console Prune 頁的 dry run 會說是誰擋的。閾值設成 prune retention 的 1.5× |
| Eventbus rows held by a stale consumer group | `eventbus_outbox_oldest_held_age_seconds` > 1d 持續 30m | offsets 表裡有 group 一天沒前進；consumer stalled 沒響就是沒人跑的 group（舊 `worker.consumer_group`、E2E 的 `test-<ts>.*`），`--forget-group` / console Prune 頁的 forget 刪掉它的 cursor |
| Reservation expiry overdue | `inventory_reservations_oldest_age_seconds{status="RESERVED"}` > 36h（1.5 × 24h TTL）持續 15m | `worker expire` 沒跑或一直失敗：看 expire CronJob 的 Job，再 `task expire -- --older-than 24h --dry-run` 或 console Expire 頁 dry run 看誰到期、sweep 有沒有 ABORTED（stock 列撞版，重跑就好）。閾值跟 Helm `expire.olderThan` 綁在一起，改一個要一起改 |
| Commit short — completed order not covered | `max(delta(inventory_reservations{status="COMMIT_SHORT"}[1h]))` > 0（severity critical，不等） | 訂單已 COMPLETED（客戶被承諾有貨），但它的 reservation 先被 expire sweep 還了庫存、`OrderCompleted` 到時庫存已被別單訂走，沒扣到：處置見下方「COMMIT_SHORT 處置」。COMMIT_SHORT 是終態列、不會被清掉，所以 alert 看的是一小時內新增的數量（最後一筆之後一小時自己解除）；累積筆數看「Reservations by status」panel |
| Saga stuck — order PENDING too long | `ordering_orders_oldest_age_seconds{status="PENDING"}` > 600 持續 5m | 有訂單等 inventory 的答覆超過 10 分鐘：看 `ordering.v1.OrderCreated`（inventory 的 group）與 `inventory.v1.StockReserved`（ordering 的 group）的 lag，再看 poison 表——handler 一直 drop 的話 eventbus 幾條都不會紅，只有這條會 |
| Payment needs attention | `sum by (alert)(increase(payment_alerts_total[15m]))` > 0 | 一張付款單進了要人處理的狀態；`alert` 標籤是 `amount_mismatch`／`capture_after_review`／`orphan_webhook`／`provider_query_stuck`／`reconcile_backlog`／`reconcile_incomplete`／`missing_total`，處理見「Payment」一節的告警表 |
| Payment reconcile falling behind | `max(last_over_time(payment_reconcile_oldest_unchecked_seconds[30m]))` > 43200 持續 15m | 最久沒被補查的付款單等了超過預設 `reconcile_window`（24h）的一半：調大 `paymentRpc.config.expire_batch` 或縮短 `paymentRpc.expire.intervalMinutes`；改了 `reconcile_window` 要一起改這個閾值 |
| Payment event unexpected | `sum(increase(ordering_payment_event_total{effect="unexpected"}[1h]))` > 0（不等） | ordering 把 `PaymentCaptured` 套到一張快照 `false` 的 `CONFIRMED` 單：`StartPayment` 會擋這種單，出現就是上游缺陷——查 `ordering.require_payment` 是否在 `CONFIRMED` 與付款之間被改、payment `consume` 是否為不該付款的單建了付款單（下方「ordering 側」的指標說明） |
| Payment reconcile gauges absent | `absent_over_time(payment_reconcile_eligible[1h])` 且 payment consume 有在報 lag，持續 30m | consume 在跑但 `expire` CronJob 一小時沒報任何 reconcile gauge：CronJob 被 suspend／一直失敗，或它的 OTLP 沒到 collector（`config.otel.endpoint`）；沒有它就沒人在補查 |
| Eventbus metrics absent | `absent(eventbus_consumer_lag)` 持續 10m | 沒有 worker 在匯出，上面幾條都是瞎的 |
| Auth credential rejections | `rate(auth_decisions_total{result="invalid"}[5m])` > 1/s 持續 5m（by `method`） | 有人在猜 key / 重放 token，或 key 換了 client 還送舊的；Loki `{service_name="rpc"} \| code="unauthenticated"` 看是哪個 procedure、request id（credential 本身不記）。rate limit 在 auth 外層，洪水先被擋 |
| Auth permission denied | `rate(rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}[5m])` > 0.1/s 持續 10m（by `rpc_method`） | 驗過身分但 role 不夠（`auth.permission_denied`）：access line 有 `sub`，是正當 operator 就補 role（`name=key:role` / JWT `roles`），不是就是在摸 admin surface |
| Identity resolver errors | `sum(rate(identity_resolver_lookups_total{result="error"}[5m])) / sum(rate(identity_resolver_lookups_total{result=~"db_ok\|db_miss\|error"}[5m]))` > 5% 持續 5m（severity critical） | resolver 查不到 DB 或 timeout：30s TTL 內驗過的 key 還能過，其他的被拒成 `auth.invalid_credential`，有 valid key 的 caller 被鎖在外面。健康時是 0，只有基礎設施故障才會有 `error`，所以門檻不用高；分母刻意只算會碰 DB 的 lookup（hit 不算）——outage 時 cache 命中照過，算進去會把比例稀釋到看不見。5m 吸收 DB failover / 連線池重建的短暫抖動。先看 Postgres，再看 Loki `{service_name="rpc"} \|= "credential resolver failed"` |

每條都帶 `__dashboardUid__` / `__panelId__`，所以「Worker — eventbus」、「Payment」、「Auth」與「Identity」的 panel 上會畫 alert 狀態。只有 rule 沒有送達：contact point /
notification policy 由部署環境依 `severity` label 自己接（Grafana → Alerting → Notification policies）。Alerting provisioning
只在啟動時讀，改完一樣 `task grafana-reload`。

踩雷：Grafana 13 的 file provisioner 用 inotify 監看資料夾，podman machine / Docker Desktop 的 bind mount 在 macOS 上
不會把事件傳進容器，所以改 JSON 不會自動重載 → `task grafana-reload`。另外 OTLP 的 metric export 間隔 60s，
`rate()` 剛啟動前兩分鐘是空的。`compose-down -v` 會清掉 Grafana 的 DB，但 provisioning 會重建 dashboard。
Retention 預設 72h（compose 的 `LGTM_RETENTION`，透過 image 的 `LOKI/TEMPO/PROMETHEUS_EXTRA_ARGS` 下 flag；Pyroscope v2 storage
還沒有時間型 retention）。Loki 突然沒 log 但 metrics / traces 正常，先看 `podman machine ssh df -h /`：VM 磁碟快滿時 Loki 的
WAL 會判 disk full、對每個 push 回 503 整批丟掉（`loki_ingester_wal_disk_full_failures_total`），跟 app 無關，`podman system prune` 就好。

### Dex dev IdP（compose / kind）

本機登入用的 OIDC IdP（[identity-login.md](identity-login.md) §10）：Dex，**只給開發與測試**——in-memory storage、固定兩個帳號、公開的密碼、沒有 TLS。`task compose-up` 會一起起（服務 `dex`，port **5556 固定**，因為 port 是 issuer 字串的一部分）；設定只有一份 `deployments/docker/dex/config.yaml`，kind 也讀它。

| | |
|---|---|
| issuer | `http://dex.localtest.me:5556/dex`（`auth.oidc.issuers[].issuer` 要與 token 的 `iss` **一字不差**） |
| client | `x-console`（public client、PKCE，沒有 secret）→ `auth.oidc.issuers[].audience: x-console`；redirect URI 列 `http://localhost:{5173,4173,8080,4203}/auth/callback`（4203＝Playwright 的 `oidc-dex` project 專用）；`web.allowedOrigins` 是同一組 origin——Console 是瀏覽器 SPA，discovery 與 token endpoint 都是跨來源 fetch，Dex 沒回 CORS 標頭時登入會停在「Sign-in failed / Failed to fetch」。新增 Console 的 origin 時兩處一起加 |
| 帳號 | `admin@example.com`、`user@example.com`，密碼都是 `password`，`email_verified=true`；`sub` 由固定的 `userID` 推出來（compose 與 kind 相同、重啟不變） |
| 本機 rpc | `configs/local.yaml` 加 `auth.oidc: {enabled: true, issuers: [{issuer: http://dex.localtest.me:5556/dex, audience: x-console, allow_insecure_issuer: true}]}`（`localtest.me` 不是 loopback 字面值，所以 http 要這個旗標；見 [config.md](config.md)） |

拿一顆真 token（authorization code + PKCE，與 Console 同一條路，不開 password grant）：

```bash
scripts/dex-token.sh                      # admin@example.com 的 access token（JWT），stdout
scripts/dex-token.sh user@example.com --id-token
curl -H "Authorization: Bearer $(scripts/dex-token.sh)" http://localhost:8080/api/...   # 需 auth.oidc.enabled；帳號要先綁定（`task dex-prebind`，見下），未綁定只能 ClaimInvite
```

**帳號預先綁定**（`task dex-prebind`，#274 part 2）：把兩個 Dex 帳號綁進 identity，**只走真流程、只打 RPC**——對每個 email：用 admin API key 找或建 account（名稱就是 email）→ `CreateInvite` → `scripts/dex-token.sh <email>` 登入 Dex → 以該 bearer 呼叫 `ClaimInvite`。刻意不加直接寫 `external_identities` 的 CLI（那會繞過 `CreateInvite` / `ClaimInvite`，是身分邊界上的後門）。

```bash
ADMIN_API_KEY=<admin key> task dex-prebind      # rpc 要在跑，auth.oidc 要開（上表）
```

- `admin@example.com` → 自己的 account `admin@example.com`，role 為 `admin`（`PREBIND_ADMIN_ROLE` 可改成非預設的 `auth.admin_role`）；`user@example.com` → 自己的 account，**沒有任何 role**。刻意不綁到 `task bootstrap` 的 root：root 是 API key 的 break-glass 帳號，混在一起之後撤銷一邊會連到另一邊。
- `ADMIN_API_KEY` 是有 admin role 的 key：`task bootstrap` 印的那把，或 config `auth.api_keys` 的 break-glass key。
- **可重跑**：以 `ListAccountIdentities` 判斷，該 email 已綁定（issuer 是 dev Dex）就跳過、不問 Dex 要 token；account 用名稱找，已有的不重建，但**先驗證**：既有 account 必須是 active 且 roles 恰為預期（admin 只有 `admin`、user 為空），否則 fail-closed（非零退出、指名是哪個 email），不改權限也不默默接受；兩個 account 都先檢查完才開始綁定；已有 pending invite 就沿用、不重發。中途失敗（例如 Dex 沒起）重跑會接著做完。
- **只能用在 dev**：`ISSUER` 不是 `http://dex.localtest.me:5556/dex` 就拒絕；`BASE_URL`（預設 `http://127.0.0.1:8080`）必須是本機（host 精確為 `localhost` / `127.0.0.1` / `[::1]`，可帶 1–65535 的 port，不得有 userinfo、path、query、fragment；整串比對而非前綴），因為 admin key 會送去那裡。要對 kind 跑就 `kubectl port-forward` 後用本機 URL。
- 不印 API key、token：key 與 token 只走 curl 的 stdin config（不進 argv / `ps`），RPC 失敗只印 HTTP status、Connect code 與 `x-error-code`，不印 response body；這三個值只在屬於已知形狀時才原樣印（status＝三位數字、code＝Connect code 的固定名單、x-error-code＝`a.b` 形的平台碼），而且不含剛送出的 credential 或裸 token，其餘一律印 `<redacted>`（server 把 key 或 token 當錯誤碼反射回來也不外洩）。

測試：`dex-prebind.sh` 是本機開發工具，沒有自動化測試（`tests/dex-prebind/stub_rpc.py` 是它原本測試用的記憶體版 IdentityService）。Playwright 走真 Dex 的 OIDC E2E 等 #295。

**名稱怎麼解析**——三個地方都必須看到同一個 issuer 字串，所以不能改寫成 `localhost` 或 service 名：

| 誰 | 怎麼到 Dex |
|---|---|
| 瀏覽器、host 上跑的 rpc / `scripts/dex-token.sh` | `*.localtest.me` 的公開 DNS 是 `127.0.0.1`，加上 compose 發佈的 `5556` |
| compose 內的容器 | 容器裡的 `127.0.0.1` 是自己，不是 host。要驗 token 的容器（例如自己把 rpc 包進 compose）照 `dex` 服務的寫法加 `extra_hosts: ["dex.localtest.me:host-gateway"]`。實測：同一個 compose network 的 curl 容器不加時連不到（`000`），加了回 `200` 且 `issuer` 相同 |
| kind 的 Pod | `deployments/helm/kind/coredns-dex.sh <ns>`（`task helm-kind-up` 會跑，可重跑）在 CoreDNS 加 `rewrite name exact dex.localtest.me dex.<ns>.svc.cluster.local`；Dex 本身是 `deployments/helm/kind/dex.yaml`，ConfigMap `dex` 由 task 從同一份 `config.yaml` 建。`deployments/helm/kind/smoke.sh` 在叢集內驗 discovery 的 `issuer` 就是這個字串 |

**真 Dex 的登入 E2E**：`task web-e2e-dex`（需要 `dex.localtest.me` 解得出來，離線見下）。它在需要時用 compose 起 `dex`，Playwright 的 `oidc-dex` project 在 port 4203 起 console，**每個測試（含 retry／`--repeat-each` 的每次 attempt）自己起一個**只信任這顆 Dex 的 `bin/rpc`（`web/apps/console/e2e/oidc-dex/rpc.yaml`、`rpc.ts`）——Dex 的兩個帳號固定、binding 跟 rpc 同壽，共用一個 rpc 的話第二次 attempt 的首次登入會是 bound；從 Dex 登入表單走到 `ClaimInvite` 綁定；帳號用 `CreateInvite` + `ClaimInvite` 真流程綁，不依賴預先綁定。CI 的 `console-e2e` job 同樣起 compose 的 `dex` 並以 `E2E_DEX=1` 跑 `task web-e2e`；`deployments/docker/dex/*` 或 compose 檔有變也會觸發該 job。

**離線**（沒有公開 DNS，`dex.localtest.me` 解不出來）：在 host 的 `/etc/hosts` 加一行

```
127.0.0.1 dex.localtest.me
```

host 上的瀏覽器、rpc、`dex-token.sh` 就都能用；容器與 kind 不受影響（它們走 `extra_hosts` / CoreDNS，本來就不靠公開 DNS）。用完刪掉那一行。

**排錯**：`scripts/dex-token.sh` 卡在 `login did not reach redirect_uri` ＝ 密碼錯，或 `CLIENT_ID` / `REDIRECT_URI` 不在 `config.yaml` 的 `staticClients`；改過 `config.yaml` 要 `podman compose -f deployments/docker/docker-compose.yaml up -d --force-recreate dex`（kind：重跑 `task helm-kind-up` 後 `kubectl -n x-monorepo rollout restart deploy/dex`）。token 的 `iss` 與設定不符（多一個 `/`、`localhost` 對 `dex.localtest.me`）→ verifier 以 `oidc: invalid token` 拒絕，細節只在 log。

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
| `orders.js` | setup 對 20 個 SKU 先 `UpsertProduct`（定價；CreateOrder 的價格是 catalog 的）再 `SetStock`（SetStock 要先有 product），每個 VU 迴圈 CreateOrder（1–3 行）→ GetOrder → ListOrders(customer) | `http_req_failed < 1%`、CreateOrder p95 < 300ms / p99 < 800ms、GetOrder p95 < 100ms、ListOrders p95 < 200ms、`rpc_errors == 0`（門檻是「筆電 + Docker Postgres 輕鬆過」的數字，要調是刻意調，不是失敗了改） |
| `ratelimit.js` | server 開 `http.rate_limit.rps=RPS`，以 `RPS × FACTOR`（預設 4）的 constant-arrival-rate 打 ListOrders | 被拒的每一筆都是 429 + `code: resource_exhausted` + `x-error-code: request.rate_limited` + 整數 `Retry-After ≥ 1`；放行數 ≈ `RPS × 秒數`（+ 一個 burst）、其餘全拒——limiter 是拒絕不是排隊 |
| `timeout.js` | server 開 `http.timeout=1ns`（要有 `postgres.url`：context 過期是在 DB call 才浮上來，in-memory repo 不看它） | 每一筆都是 504 + `code: deadline_exceeded` + `x-error-code: request.timeout`，不是 INTERNAL（pgx 包過的 context error 也要被對應到）；server 沒斷線 |
| `reservation.js` | 旁邊要有一個 `worker`（同一個 Postgres）：每個 VU CreateOrder（1 行、qty 1）後每 `POLL`（100ms）打一次 GetReservation，404 視為「還沒消費」 | `reservation_latency`（CreateOrder 回來 → 看到 RESERVED）p95 < `RESERVE_P95`（預設 3000ms；outbox 是 polling，空輪詢之後最多再等約一個 `worker.poll_interval`（預設 200ms）才看到新事件，筆電 10 VU 實測 p50 ≈ 0.2s / p95 ≈ 0.25s；改這項之前是 library 預設的 1s，p50 ≈ 0.9s / p95 ≈ 1.1s。腳本的 `POLL`（100ms）也佔解析度，所以數字會落在 100ms 的倍數附近）、`RESERVE_TIMEOUT`（30s）內沒等到算 `reservation_timeouts`、不是 RESERVED 算 `reservation_wrong_status`，兩者都要 0 |

```bash
task k6 -- orders                 # 對 http://127.0.0.1:8080（task run）跑 60s × 20 VU；BASE_URL / API_KEY / VUS / DURATION / RPS 用 env 蓋（auth 開著就帶 API_KEY；`orders` / `reservation` 的 setup 先 `UpsertProduct` 再 `SetStock`，auth 開著時要 admin role：API_KEY 本身是 admin key 就夠，否則另帶 `ADMIN_API_KEY` 只給 setup 用）
task k6 -- ratelimit -e RPS=50    # server 要先用 APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50 起
task k6 -- reservation            # server 旁要有 bin/worker 在跑（同一個 Postgres），不然每筆都 timeout
task k6-auth-matrix               # tests/k6/auth-matrix.sh：orders / reservation 在每一種 auth 狀態各跑一次（2 VU × 5s）加上 fixtures，見下方「Auth 組合」；CI 的 k6-auth job 就是它
task k6-wire                      # tests/k6/wire.sh：四個腳本打 tests/k6/wire_server.py（回壞掉的 HTTP response），驗 key 不進 k6 的輸出 / summary / metrics tag，見下方「壞掉的 HTTP response」；不需要 Postgres 和 rpc，CI 的 k6-wire job 跑它
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

CI 的 `k6-auth` job 和 `k6` job 平行，只在 auth 相關路徑（`pkgs/platform/server`、`pkgs/platform/config`、`pkgs/identity`、`apps/rpc`、`configs`、`tests/k6`）變動時跑；壞掉的 HTTP response 那組由獨立的 `k6-wire` job 跑，跟所有產品 Go 變動；matrix 的 cell 名字可以接在 task 後面只跑幾個：`task k6-auth-matrix -- on-admin-orders whoami-public`。其他沒有列的組合（JWT、`optional` 模式、其他 procedure 的 public）沒有驗證，不保證。

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
- lag 持續 > 0 且 `oldest_age` 遠大於間隔：瓶頸是 handler 吞吐，不是輪詢；縮間隔沒用，先看 handler。`postgres` broker 下加 replica **沒有量到**變快（同一個 consumer group 的 2 個 replica 清 2000 筆積壓，中位數 6.00s，1 個是 5.72s，見下節「Subscriber 批次大小」）；要靠加 replica 擴充，改用 `worker.broker: redis`（本文沒有量 redis 模式的吞吐）。
- Postgres 的 `pg_stat_database.xact_commit` 在閒置時就很高、或連線數吃緊：放大間隔（如 500ms–1s），延遲換 DB 負載。
重送（nack）與查詢失敗後的等待仍是 library 的 1s：handler 失敗由 `Register` 的 Retry 在行程內重試，不靠 nack；DB 出錯時也不該更頻繁地打它。

設定：`worker.poll_interval` / `APP_WORKER_POLL_INTERVAL`（Helm `config.worker.poll_interval`），必須 > 0。

### Subscriber 批次大小（`SubscribeBatchSize`，量測結果：不調整，沒有設定）

watermill-sql 一次輪詢最多領 100 筆（`SubscribeBatchSize`，library 預設），這些訊息在同一個 transaction 裡一筆接一筆處理，cursor 最後才前進。#363 問：把它調小（10、1）能不能縮短 `reservation_latency`。**判定規則在量之前就固定**（#363），結果是**不達標，所以不加 `worker.subscribe_batch_size`、預設維持 100**。

**方法**（同一台機器、同一個 commit、`worker.broker: postgres`、`worker.poll_interval` 預設 200ms；只有批次大小不同）：
- 候選 100、10、1，照固定順序 `100→10→1→1→10→100→100→10→1` 跑三輪（每輪 3 個候選），每次執行都是新建的 database：`migrate up`、catalog-rpc、rpc、worker 起停，跑完 DROP。
- 指標 A：`k6 run tests/k6/reservation.js -e VUS=50 -e DURATION=60s` 的 `reservation_latency` p95。
- 指標 B：worker 停著，先建 2000 筆訂單（全部 PENDING），再啟動 worker；計時從啟動 worker 到「沒有 PENDING、全部 CONFIRMED、RESERVED 的 reservation 數等於訂單數」（`tests/k6/bench_drain.sh`，輪詢間隔 0.2s）。
- **採用條件**：候選的 A 在三輪都比同一輪的 100 改善至少 20%，且 B 變慢不超過 25%。

**批次確實有累積，而且 knob 確實生效**：另外各跑一次插了計數的 worker（VUS=50；batch 100 跑 60s，batch 10 與 1 跑 20s；都不在下面 9 輪裡）。計數掛在 `OffsetsAdapter.AckMessageQuery`，記每次領到訊息的輪詢的 `len(p.Rows)`：

| batch | 領到訊息的輪詢次數 | 每次平均筆數 | 最多 | 領到 2 筆以上的比例 |
|---|---|---|---|---|
| 100 | 290 | 34.14 | 50（= VUS） | 97% |
| 10 | 504 | 8.86 | 10 | 97% |
| 1 | 4944 | 1.00 | 1 | 0% |

所以這個負載下 batch 100 與 10 確實會累積；batch 1 的上限就是 1，沒有累積。

**環境**：Apple M1 Max（10 核、32 GB）、macOS 26.6.2（Darwin 25.6.0）、Postgres 17.11（podman 上的 compose Postgres，與 rpc / worker / k6 同一台機器）、Go 1.27.1、k6 v2.2.0。**量測時這台機器不安靜**：1 分鐘 load average 在 13 到 35 之間（10 核），超過 `bench.sh` 的 1.0/核門檻；這是共用的開發機，沒有等到安靜。每次執行前後的 load 在下表。

**怎麼重做**（量測用的暫時程式與 harness 沒有留在 repo，因為 `main` 沒有這個設定，留著跑不起來）：
1. 暫時讓 `eventbus.NewPostgresSubscriber` 把批次大小傳給 watermill-sql 的 `DefaultPostgreSQLSchema.SubscribeBatchSize`（`SubscriberConfig.SchemaAdapter`），由環境變數 `APP_WORKER_SUBSCRIBE_BATCH_SIZE` 帶進 worker（worker 的 handler subscriber 與 redis 模式的 forwarder 都經過 `NewPostgresSubscriber`）。要驗它生效，在 `OffsetsAdapter.AckMessageQuery` 記 `len(p.Rows)`，結果要像下面的計數表（batch 1 最多 1、batch 10 最多 10）。
2. 每次執行：在 compose Postgres 上新建 database → `migrate up` → 起 catalog-rpc（`tests/k6/catalog.sh`）→ 起 rpc → 起 worker（只改批次大小，其他設定全是預設）→ 量 → DROP database。`worker.poll_interval` 維持預設 200ms，auth 關閉。
3. 指標 A：`k6 run tests/k6/reservation.js -e VUS=50 -e DURATION=60s`（加 `-e RESERVE_P95=3600000` 讓延遲門檻不擋，其他 correctness 門檻不變），取 `--summary-export` 的 `reservation_latency` p95。
4. 指標 B：不啟動 worker，先用 k6 `shared-iterations` 對 CreateOrder 建恰好 2000 筆訂單（20 VU，每筆一行、數量 1、20 個 SKU，庫存 1,000,000），確認 2000 筆都是 PENDING；啟動 worker 後立刻呼叫 `tests/k6/bench_drain.sh <database url> <out> 600`，取輸出的 `seconds`。
5. 順序固定為 `100,10,1,1,10,100,100,10,1`（A 全部跑完再跑 B）；replica 對照是 batch 100，replica 數 `1,2,2,1,1,2`，同一個 consumer group。
6. 判定規則同上，不因結果調整。要在安靜的機器上重量，執行前先確認 `uptime` 的 load average 低於核數。

**指標 A：`reservation_latency` p95（ms）**，括號是相對同一輪的 100：

| 輪 | 100 | 10 | 1 | load（各次執行前→後） |
|---|---|---|---|---|
| 1 | 431.05 | 412.00（-4%） | 731.10（+70%） | 13.39→14.47、14.67→15.85、15.38→23.36 |
| 2 | 843.85 | 847.00（+0%） | 633.40（-25%） | 23.36→22.78（1）、22.78→27.59（10）、27.59→21.62（100） |
| 3 | 9063.60 | 440.80（-95%） | 523.85（-94%） | 21.62→35.35（100）、32.92→22.67（10）、21.17→21.26（1） |

**指標 B：2000 筆積壓清空秒數**，括號是相對同一輪的 100：

| 輪 | 100 | 10 | 1 |
|---|---|---|---|
| 1 | 6.62 | 6.62（+0%） | 8.90（+34%） |
| 2 | 6.66 | 6.66（+0%） | 10.77（+62%） |
| 3 | 6.31 | 7.42（+18%） | 11.05（+75%） |

**判定**：候選 10 在第 1 輪只改善 4%（< 20%）；候選 1 在第 1 輪反而慢 70%。兩者都沒有三輪都改善至少 20%，所以不採用，B 不必再判。候選 1 的 B 也三輪都慢了 34% 以上（超過 25%）。規則沒有允許剔除離群值，所以 100 在第 3 輪的 9063.60ms（load 35）照算；它讓 10 和 1 在第 3 輪看起來進步 94% 以上，**這一輪不能當進步的證據**，只說明在這個環境下 p95 的雜訊遠大於 20%。要再驗，得在安靜的機器上用同一組規則重跑。

**順帶量：同一個 consumer group 開 2 個 replica 跑指標 B**（batch 100，順序 1、2、2、1、1、2 個 replica，秒）：1 個 replica 為 7.05、5.72、5.69（中位數 5.72）；2 個 replica 為 6.00、5.70、6.63（中位數 6.00）。**沒有量到 2 個 replica 比較快**。每個 replica 的 cursor 列是 `FOR UPDATE`（同一個 topic 的輪詢要輪流領），這是對機制的推演，沒有另外驗證；清完 2000 筆只要 6 秒左右、機器又不安靜，這組樣本也看不出小於一兩秒的差異。所以上面「要不要調」的 replica 建議改成指向 `worker.broker: redis`。

## Payment（`payment-rpc`，#367）

`apps/payment-rpc` 一個 binary、三個子命令，部署時各是一個 workload（image 是切片 5a，chart 是 5c，見下面「chart 怎麼部署 payment-rpc」；本機手動跑 `./bin/payment-rpc <cmd>`）：

| 子命令 | 形狀 | 做什麼 | 沒跑會怎樣 |
|---|---|---|---|
| `serve` | 常駐 | Connect `PaymentService`（`StartPayment`／`GetPayment`，只認 service key `rpc`）＋ webhook route `POST /api/payment/webhook`（簽章，不認 API key） | 顧客付不了款；provider 的通知會被 provider 重送到它放棄 |
| `consume`（binary 是 `payment-worker`；`payment-rpc consume` 在 #410 切片 2 合併後發布的第一個 image 起不再存在，更早的 `payment-rpc` image 仍有） | 常駐，不聽埠 | 訂閱 `OrderConfirmed`／`OrderCancelled`／`OrderCompleted`（建付款單、終結付款單、必要時要求退款）、`PaymentRefundRequested`（呼叫 provider 退款）、`WebhookReceived`（套用已落地的通知） | 付款單不會建、通知不會套用、退款不會做；事件在 outbox 裡等，不會遺失 |
| `expire` | 跑一次就結束（CronJob，週期 = `payment.expire_interval`） | 逾時的 `PENDING` 先問 provider 再決定 `EXPIRED`／`CAPTURED`／`NEEDS_REVIEW`；`EXPIRED`／`CANCELLED` 在 `payment.reconcile_window` 內補查；順手刪逾期且已處理的 `webhook_events` | 逾時付款單不會過期、webhook 缺席的款項不會被發現 |

三個都要 `postgres.url`（`--require-durable` 另要求 `serve` 的 `auth.mode: required` 與單一把 `rpc` key）。`consume` 把消費位置放在 **ordering** 的三張事件表上，所以 `task migrate -- up` 必須先把 ordering 與 payment 都跑完，否則 `consume` 啟動時以 `relation … does not exist` 結束（fail closed，不是靜默略過）。

**告警**：`payment_alerts_total{alert}`（process log 同時有一行 ERROR `payment alert`，欄位只有固定類別與 order id）。類別與處理：

| `alert` | 意思 | 怎麼處理 |
|---|---|---|
| `amount_mismatch` | provider 說收到的款，金額或幣別與付款單不符；付款單進 `NEEDS_REVIEW` | 到 provider 後台對帳；系統不再動它（第一版沒有系統內出口） |
| `capture_after_review` | `NEEDS_REVIEW` 的付款單又收到收款通知 | 同上 |
| `missing_total` | 收到沒有 `total` 的 `OrderConfirmed`（舊事件）；沒有建付款單 | 查發布者；舊事件不能當成 0 元 |
| `orphan_webhook` | 驗過簽章的收款通知，沒有任何付款單開過那個 checkout | 到 provider 後台查那筆款；通知已標為處理完，不會重試 |
| `provider_query_stuck` | 逾時的 `PENDING` 連續三輪沒能問到 provider，仍是 `PENDING` | 查 provider 狀態與憑證；恢復後下一輪自動收斂 |
| `reconcile_backlog` | 補查落後：窗口內有付款單超過 `reconcile_window` 的一半沒被問過 provider（`payment_reconcile_oldest_unchecked_seconds` 超過閾值）。`expire_batch` × 窗口內的輪數已經不夠覆蓋待查的筆數，沒被問到的單有款也看不到 | 看 `payment_reconcile_eligible`（待查筆數）：調大 `payment.expire_batch` 或縮短 CronJob 週期與 `payment.expire_interval`；同時查 `provider_query_stuck`（卡在查詢失敗的單也會佔輪次）。這是補查「有條件」保證的觀測口（`docs/payment.md` §3） |
| `reconcile_incomplete` | 一張 `EXPIRED`／`CANCELLED` 付款單最後一次補查失敗，補查窗口即將關閉 | 到 provider 後台確認有沒有款（已知限制：窗口後才到的款，沒有 webhook 就不會被發現） |

**補查的游標與 gauge**：`payment.payments.swept_at` 是 `expire` 的游標（最久沒被看過的先查，查過就推進，不論 provider 回什麼；不是付款單狀態，改它不動 `version`）。`payment_reconcile_eligible`（窗口內待查筆數）與 `payment_reconcile_oldest_unchecked_seconds`（最久沒被看過的等了多久）每輪 `expire` 結束時各記一次；CronJob 是短命 process，要靠 OTLP flush 在結束時送出，沒收到資料就等於沒有量測，不能當成「沒有落後」。

**migration 的鎖（payment `0003`）**：`0003_mock_provider` 只 `CREATE TABLE` 兩張新表（`payment.mock_checkouts`、`payment.mock_refunds`，後者以外鍵指向前者），不 `ALTER` 任何既有的表、不建索引在既有的表上，也不參照 `payment.payments`，所以**不會讓 `payments` 等待**：跑的時候讀和寫照常（實測：`TestMigrationLock_NewTablesDoNotBlockPayments` 用真正的 migration runner 跑一個同形狀的檔案——兩張新表、一個外鍵、`pg_sleep`、最後失敗——檔案還在 sleep 時對 `payments` 的 `SELECT` 與 `UPDATE` 在 `lock_timeout` 內完成；`TestMigrationLock_0003OnlyCreatesNewTables` 逐句檢查真正的 `0003` 只有這兩個 `CREATE TABLE`）。檔案仍是**一個交易**，失敗整個 rollback、不留表。可以在服務運行中跑；但新版 `serve`／`consume`／`expire` 在 `postgres.url` 設了之後會讀寫這兩張表，所以**先 `migrate up`，再換 payment-rpc**（舊 binary 不碰這兩張表，順序反過來則 mock 的呼叫回「不可用」，fail safe）。這兩張表的資料保留見 `docs/payment.md` §3：只給 demo／kind，不歸任何 prune。

**migration 的鎖（payment `0004`）**：`0004_mock_faults` 只 `CREATE TABLE payment.mock_faults` 一張新表，不 `ALTER` 任何既有的表、不建索引、不參照 `payment.payments`，所以和 `0003` 一樣**不讓 `payments` 等待**，可以在服務運行中跑（`TestMigrationLock_0004OnlyCreatesANewTable` 逐句檢查檔案只有這一個 `CREATE TABLE`；檔案仍是一個交易）。新版 `serve`／`consume`／`expire` 只在 `payment.mock_control` 開著時才讀這張表，所以**先 `migrate up`，再換 payment-rpc**；`mock_control` 預設關，沒開的部署從不碰它。Rollback 見下「只退 payment `0004`」。

**mock 控制入口（`payment-rpc mock`，切片 6，只給 kind 與測試）**：沒有真人付得了款的 mock provider 要走完整流程，需要有人扮演顧客與 gateway，這就是 `payment-rpc mock pay|notify|fail`。它**不是 HTTP route**（沒有新的對外入口，`TestMockControl_OpensNoRoute`），是一個要能連到資料庫的 CLI，讀寫 `serve`／`consume`／`expire` 共用的 `payment.mock_*` 表。三道門，缺一個就拒絕、什麼都不印、什麼都不改，錯誤訊息指名要改的設定：`payment.provider` 是 `mock`、`payment.mock_control: true`（`APP_PAYMENT_MOCK_CONTROL`，預設 `false`，chart 預設不開，kind profile 才開）、`payment.mock_webhook_secret` 非空（空的話它簽的 webhook 沒有任何 process 驗得了）。

- `mock pay --order <id> [--amount N --currency C] [--deliver <url>]`：把該訂單付款單的 checkout 標成已付（`--amount` 付別的金額，演練 T4／T9），印出簽好的 `payment.captured` webhook（一行 JSON：`ref`、`event_id`、`body`、`signature`）；加 `--deliver` 就直接 `POST` 到 `serve` 的 `/api/payment/webhook`（非 2xx 讓指令失敗，不印東西）。同一個 `pay` 跑兩次是重送（同 event id、同 bytes）。
- `mock notify --ref <ref> --id <event-id> --type <type> [--amount N --currency C] [--deliver <url>]`：簽任何型別、任何 id 的 webhook 而不動 checkout：重送、亂序（先送後來的）、未知型別。
- `mock fail on|off [--op create_checkout,get_payment,cancel_checkout,refund]`：讓 provider 的指定操作（預設全部）失敗或恢復。故障存在 `payment.mock_faults`，**每個 `payment.mock_control` 開著的 `serve`／`consume`／`expire`** 的 mock 在每次呼叫時讀它（呼叫以 provider 不可用失敗，不洩漏任何內容；對 payment 來說就是 provider 不通）；沒開的 process 忽略它，所以一列遺留的 fault 影響不到 production。
- 在 kind 裡，從 `serve` 的 pod 執行（image 是 distroless，entrypoint 就是 binary `/app`，沒有 shell；`--deliver` 由 binary 自己送；pod 的 env 已有資料庫與簽章金鑰）：`kubectl exec deploy/<release>-payment-serve -- /app mock pay --order <id> --deliver http://127.0.0.1:8080/api/payment/webhook`。`payment.mock_*` 的資料不歸任何 prune。
- 期限：指令只有**一個**期限（`mockTimeout`，30 秒），開連線與每一條 SQL 共用，不是每一步各算一次。到期時，卡在鎖上的 statement 由連線向 server 送 cancel request 取消（SQLSTATE `57014`，連線池用 `postgres.NewDBContext`），指令回 `deadline exceeded`，**回報時 server 上已沒有等待的 backend**，釋鎖後也不會補寫。測試（都走真入口 `run`）：`TestMockControl_EverySubcommandEndsAtItsDeadlineWhileTheDatabaseWaits`（`pay`／`notify`／`fail` 各持一張表的鎖）、`TestMockControl_DeadlineIsOneForTheWholeCommand_SlowLookupThenWrite`（先等 lookup 約過半預算、再等寫入）、`TestMockControl_DeadlineClosesAConnectionWhoseHandshakeNeverCompletes`（對端收到 StartupMessage 後不回，回報時連線已關）；各有正控制。範圍：連線建立中沒有 backend 可取消，期限到只能關掉 socket；server 若在 2 秒內沒回應 cancel request，連線也是被關掉，這時**不能**說 server 端的 statement 已結束（這一格沒有測試）。只有這個 CLI 用這條路；`serve`／`consume`／`expire` 與其他 app 的連線池行為不變。

**`ordering.require_payment` 開著時 worker 也要有 `payment.upstream`**（切片 6）：worker 是取快照的地方，開關開著而沒有 payment-rpc 可付，新訂單會停在 `CONFIRMED` 等一筆沒人能發起的付款，直到預留 TTL（24 小時）才被取消。所以 rpc 與 worker 在這個狀態都**拒絕啟動**（同一條規則 `config.CheckRequirePayment`，在連資料庫之前），錯誤訊息指名 `payment.upstream`（`APP_PAYMENT_UPSTREAM`）。worker 只要求 upstream（只檢查有設，不呼叫）、**不要 `payment.api_key`**：那是 rpc 的 service credential，給 worker 等於讓被攻破的 worker 能冒充 rpc 以任何顧客的身分呼叫 payment-rpc。這個檢查**不證明 payment-rpc 活著**——活不活由 payment 的告警管。Helm chart 在 render 時另有「`require_payment` 開著而 `paymentRpc.enabled` 是 false」的失敗；binary 這道門是給 chart 以外的部署（compose、手寫 manifest）用的。

**migration 的鎖（payment `0002`）**：`0002_saga` 在**一個交易**裡跑（golang-migrate 把整個檔案當一次 query 送出；失敗時整個檔案 rollback，不留半套）。第一個動 `payment.payments` 的是 `ALTER TABLE … ADD COLUMN swept_at`：可為 NULL、沒有 DEFAULT，所以不重寫表，但它取 **ACCESS EXCLUSIVE，持有到交易 commit**——不是只有它自己執行的那一瞬間。之後三個一般（非 `CONCURRENTLY`）`CREATE INDEX` 在同一個交易裡建，所以 **`payments` 在整個檔案跑完前，讀和寫都會等**：`SELECT` 也會被擋住（實測：`TestMigrationLock_AddColumnBlocksReadsUntilTheFileEnds` 用真正的 migration runner 跑一個同形狀的檔案，檔案還在 sleep 時 `SELECT` 就等到 `lock_timeout`；檔案失敗後欄位不留）。等待時間 = 取鎖的等待 + 三個索引的建置時間，隨列數成長；這不是零鎖的線上變更。取鎖本身也有代價：`ADD COLUMN` 要等到所有已開著、碰過 `payments` 的交易結束才拿得到鎖，而它排隊期間，後來的讀寫也都排在它後面（實測：`TestMigrationLock_NoRewriteAndTheQueueBehindAddColumn`，同時確認它不重寫表——relfilenode 不變）。`payments` 在切片 2 才出現，列數預期不多，但沒有量測過實際建索引的時間，所以不要拿「列數少」當保證。建議在 `serve`／`consume`／`expire` 停著或流量低時跑，並設 `lock_timeout`，拿不到鎖就失敗（失敗整個檔案 rollback，可以重跑）。

**卡住的退款**：`PaymentRefundRequested` 重試三次（100ms 起、指數退避）後進 `payment.events_poison`，付款單維持 `REFUNDING`。provider 恢復後 `requeue` 那一列（`task poison -- list payment` 看、`task poison -- requeue payment <offset>` 送回；admin RPC 與 console 的 Poison 頁同樣涵蓋 payment），退款以付款單 id 為冪等鍵，不會退兩次。

**`webhook_events` 的 retention**：不歸 `worker prune`（它只管事件表與 inbox）。由 `payment-rpc expire` 順手刪「已處理」且早於 `payment.webhook_retention` 的列，每輪最多 `payment.expire_batch` 列；未處理的、`NEEDS_REVIEW` 付款單的通知永遠不刪。這張表是重送通知的去重記憶，所以 `webhook_retention` 必須大於 provider 的重送窗口（mock 是 72h，`payment.mock_redelivery_window`），否則**拒絕啟動**；`0` = 永遠保留。

**chart 怎麼部署 payment-rpc（切片 5c，`paymentRpc.enabled`，預設 off）**：三個 workload——`<release>-payment-serve`（Deployment，Connect `PaymentService` 與 webhook route，同一個 port，image `…/payment-rpc:<tag>`）、`<release>-payment-consume`（Deployment，沒有 listener、沒有 probe，**image `…/payment-worker:<tag>`、container `payment-worker`、args `--require-durable`**，#410 切片 2 起；之前是 `payment-rpc consume`）、`<release>-payment-expire`（CronJob，payment-rpc image）。**關閉時 render 與這一版之前逐位元組相同**（實測：七個舊 profile 的 `helm template` 輸出，扣掉 chart 版本字串，`diff` 為空；`check.py` 另斷言關閉時沒有任何 payment 物件、rpc 沒有 `APP_PAYMENT_*`），所以升級到有這個 chart 的版本不改變既有部署。
- **自己的 ConfigMap `<release>-payment-rpc` 與 Secret `<release>-payment-rpc-env`**，同 catalog-rpc 的理由（它遇到 `auth.jwt`、`auth.oidc.enabled`、auth 開但沒有 api key 就拒絕啟動，而 release 共用的 config／Secret 正是 rpc 的 OIDC issuer、JWT secret、使用者 API key 所在）：config 只有 `http`／`postgres`／`log`／`otel`（不帶 `otel.service_name`）、`auth: {mode: required, admin_role}`、`worker`（**broker 固定 `postgres`**：`consume` 讀 ordering 的 Postgres outbox，Redis broker 它自己會拒絕，所以即使 release 的 worker 用 Redis，這份也是 postgres）、`payment`（`provider: mock`、`expire_interval`，與 `paymentRpc.config` 的 `window`／`reconcile_window`／`webhook_retention`／`expire_batch`／`mock_checkout_ttl`／`mock_redelivery_window`）。Secret 的 key：`APP_POSTGRES_URL`、`APP_AUTH_API_KEYS=rpc=<apiKey>:<admin role>`、`APP_PAYMENT_MOCK_WEBHOOK_SECRET`，以及 `APP_PAYMENT_API_KEY`（rpc 拿的是**同一個**值的副本，不會漂開）。要自己管 Secret：`secrets.paymentRpc.existingSecret`（四個 key，與 `apiKey`／`mockWebhookSecret` 擇一）。三個 workload 各自只拿需要的 key：`serve` 三個（Postgres、service key、mock secret），`consume` 與 `expire` 兩個（沒有 listener，不需要 service key）；**全部以 `secretKeyRef`（`optional: false`）進 container，不用 `envFrom`**。
- **`--require-durable` 是 container arg**（`serve`／`consume`／`expire` 都有）：extraEnv 與 Secret 改不了它。`serve` 另外要求 `auth.mode: required` 與 `auth.api_keys` 恰好一把、名為 `rpc`，在**最終有效設定**上判斷；chart 在 render 時先擋能擋的，binary 擋 render 看不到的（外部 Secret 的值是空字串、有人用 env 把 auth 關掉）。
- **護欄逐一列四個入口**（`ci-negative/` 的 `payment-rpc-*` 案例逐格測「安全條件被改掉」）：① values——`paymentRpc.config` 的數字要自洽（`window` ≤ `mock_checkout_ttl` < `reconcile_window`，`webhook_retention` 為 0 或大於 `mock_redelivery_window`，與 binary 啟動時的檢查相同），`serve.service.type` 只能是 `ClusterIP`（這個 port 同時載著內部 RPC），`expire.intervalMinutes` 要整除 60（排程 `*/N * * * *` 與 `payment.expire_interval: Nm` 由這同一個值產生，不會漂開）；② existingSecret——不能解析成 release 的 Secret（`secrets.existingSecret`、`<fullname>-env`、`<fullname>-migrate-env`）或 catalog-rpc 的 Secret，**兩個方向都擋**（共用的或 catalog 的 `existingSecret` 取了 `<fullname>-payment-rpc-env` 也 render 失敗），比的是最終名稱；③ extraEnv——`paymentRpc.{serve,consume,expire}.extraEnv` 不能設 `APP_AUTH_*`、`APP_PAYMENT_*`、`APP_WORKER_*`、`APP_REDIS_*`、`APP_POSTGRES_URL`、`APP_CONFIG`（任何大小寫）；`rpc.extraEnv` 與 `worker.extraEnv` 不能設 `APP_PAYMENT_*`（**無論 payment-rpc 開不開**：rpc 的上游位址與 key 是 chart 的，worker 永遠拿不到 provider 憑證）；④ env 優先序——extraEnv 排在前面、chart 的排在後面，重複名稱取最後一個，所以 chart 的值贏（`check.py` 斷言順序與沒有重複名稱）。另外 `config.ordering.require_payment: true` 沒有 `paymentRpc.enabled`、或 `config.auth.mode: off`（沒有 credential 就沒有人能付款，訂單會一直停在 `CONFIRMED`）都 render 失敗；開關的預設值不動。
- **ingress 只對外開 webhook**（`paymentRpc.ingress`，預設 off）：每個 host 一條 rule、一條 path——`/api/payment/webhook`，`pathType: Exact`；Connect RPC（同一個 port）、`/api/metrics`、probe、`/api/docs` 都不在 ingress 上（`task helm-check` 負例：渲染出的 Ingress 規則只含 webhook path，也沒有任何別的 Ingress 指到 serve Service；kind 實測見下）。這條 route 沒有 API key、沒有 `http.rate_limit`（簽章是它的憑證，`docs/payment.md` §7），所以限流在 ingress：**渲染成 ingress-nginx 的 annotation**（`limit-rps`、`limit-burst-multiplier`、`limit-connections`，加上 `proxy-body-size: 1m` 對應 webhook 1 MiB 的上限）。沒有 `className`、沒有 host、任一個限制不是正數都 render 失敗；`paymentRpc.ingress.annotations` 不能碰 `limit-*`、`proxy-body-size`、`*-snippet`、`rewrite-target`、`use-regex`、`app-root`（它們能放寬限制或多開路徑）。**只支援 ingress-nginx 的 annotation**：換別的 controller 要自己加對應的限流，chart 不會替它翻譯。限流是 per client IP（provider 的出口 IP 少時要留意 burst）。`networkPolicy.enabled` 時 `serve` 只收同 release 的 rpc pod 與 `networkPolicy.paymentRpc.ingressFrom`，**列表空而 ingress 開著會 render 失敗**（webhook 從 ingress controller 進來，不列它就全被擋）；`consume`／`expire` 沒有 listener，一律 deny all ingress。
- **provider credential 只有 payment 的 process 拿得到（payment-rpc；#410 起還有 payment-worker，chart 切片 2 才部署它；最終有效設定，三個層次）**：provider 專用欄位清單（`config.AppConfig.ProviderCredentials()`）目前只有 `payment.mock_webhook_secret`；加新 provider 的 credential 欄位時一起加進這份清單。矩陣——**入口 × 行為**：（a）`config.payment.*`（values 寫進共用 ConfigMap）：chart render 失敗（`mock_webhook_secret`、`api_key` 都是，`ci-negative/config-payment-*`），`check.py` 另斷言共用 ConfigMap 的 `payment` 沒有這兩個 key；（b）`worker.extraEnv`／`rpc.extraEnv` 的 `APP_PAYMENT_*`：render 失敗；（c）共用 `secrets.existingSecret`（worker 與 rpc `envFrom`）或 config 檔裡有 `APP_PAYMENT_MOCK_WEBHOOK_SECRET`／`payment.mock_webhook_secret`：render 看不到（外部 Secret 的內容），由 binary 擋——**worker 的每個 command（consumer、prune、expire、poison、bootstrap）與 rpc 在 `config.Load` 之後的最終值有非空的 provider credential 就拒絕啟動**，訊息點名設定、不印值（`apps/worker/provider_credential_test.go`、`apps/rpc/provider_credential_test.go`，env 與 config 檔兩個入口）；（d）rpc 合法需要的 `payment.upstream`／`payment.api_key`／`payment.timeout`（env 進來，api_key 從 payment-rpc 的 Secret 以 secretKeyRef）與非機密的 provider 調整（`provider`、`window`、`mock_checkout_ttl` 等）不是 credential，照常可用（測試有正控制）。反方向：`payment-rpc --require-durable`（和 `payment-worker --require-durable`）在 `serve`／`consume`／`expire` 都在 `config.Load` 之後檢查 `payment.mock_webhook_secret` 不可為空或全空白（external Secret 的 key 存在但值是空的，`optional: false` 擋不住；`TestRun_RequireDurable_MockWebhookSecret` 對三個 command 各測 missing／empty／blank／tab-newline 與一個正常 Secret 的正控制）。
- **`consume` 的 replica 與 autoscaling**：預設 **1 個，沒有 autoscaling**。同一個 consumer group 的 replica 會分工，但 **`postgres` broker 下加 replica 沒有量到變快**（見「Worker 輪詢間隔」的「要不要調」：2 個 replica 清 2000 筆積壓中位數 6.00s，1 個是 5.72s），而 `consume` 只有 Postgres broker，所以多開 replica 是為了可用性（重啟時另一個還在），不是吞吐；chart 不宣稱加 replica 會變快。**它的 consumer group 是 `payment-rpc`（實際名稱 `payment-rpc.payment`），刻意不在 worker 的 `worker.*` 底下**：`worker.autoscaling` 的 lag 查詢是 `max(eventbus_consumer_lag{consumer_group=~"worker\\..*"})`，如果 payment 用預設的 `worker.payment`，payment 的積壓會讓 **worker** 擴張（`check.py` 斷言 group 不落在 worker 的查詢底下）。
- **觀測**：Grafana dashboard 的 **Payment** 列（`deployments/docker/grafana/dashboards/rpc.json`，面板 45–50）：`payment_reconcile_eligible`、`payment_reconcile_oldest_unchecked_seconds`、`payment_alerts_total{alert}`、payment 自己 consumer group 的 `eventbus_consumer_lag`／`eventbus_consumer_oldest_age_seconds` 與 `eventbus_poison_messages`。Alert（`alerting.yaml` 的 `payment` group，四條）：`Payment event unexpected`（3B 的 `unexpected` 指標，見「ordering 側」）、`Payment needs attention`（`payment_alerts_total` 15 分鐘內有增加）、`Payment reconcile falling behind`（最久沒補查的付款單等了超過 43200s = 預設 `reconcile_window` 的一半；改了 `paymentRpc.config.reconcile_window` 要一起改）、`Payment reconcile gauges absent`（consume 在報但 CronJob 一小時沒報任何 reconcile gauge；**沒有量測不等於沒有積壓**，所以單獨一條）。payment 的 consumer lag／poison 由既有的 `worker-eventbus` 規則涵蓋（它們對所有 group 取 max）。CronJob 是短命 process，gauge 靠結束時的 OTLP flush，**要設 `config.otel.endpoint`**；規則用 `last_over_time` 讀（一個 5 分鐘就 stale 的 instant 值會讓 CronJob 週期 15 分鐘時大半時間看不到）。`task helm-check` 的 `check-observability.py` 驗面板與規則存在、alert 連到 Payment 列的面板，且 metric 名稱確實是程式碼匯出的（`payment_*` 是 instrument 名，`eventbus_*` 在 `platform/eventbus/metrics.go` 列出的 Prometheus 名單裡）——打錯字的規則永遠不會觸發，這是唯一擋住它的地方。
- **kind 實測**（`task helm-kind` 的 `deployments/helm/kind/payment.sh`，真的 kind cluster）：serve ready 且 RPC 要 service key（沒帶 401、帶錯 401、帶對 404 `payment.not_found`）；consume 起得來（log `payment-worker started`）；從 CronJob 起一個 Job（`create job --from=cronjob/…`）跑完成功；裝 ingress-nginx（chart `4.14.3`，admission webhook 關掉）之後，webhook 從 ingress 打得到（沒簽章 → 400，是 payment-rpc 回的），**Connect RPC（含帶著正確 service key，`/payment.v1…` 與 `/api/payment.v1…` 兩種路徑）、`/api/metrics`、`/api/readyz`、`/api/healthz`、`/api/version`、`/api/docs`、webhook 路徑的變形與別的 Host 都是 404**（對照：同樣的 metrics／healthz 直接打 Service 是 200，所以 404 是 ingress 沒有路由，不是後端沒有）；40 個併發 webhook 請求有 400 也有 503（限流真的在作用），5 秒後放行；2 MiB body 被擋（413）；把 serve／consume 縮到 0 個之後建單，訂單照常到 `CONFIRMED`；最後還原。**沒有證明的**：NetworkPolicy（kind 的 kindnet 不實作，`check.py` 只驗形狀）、ingress-nginx 以外的 controller、真實 provider 的 webhook 來源 IP 與限流的相互作用、`consume`／`expire` 在 mock 以外的 provider 下的行為（第一版只有 mock）。完整的付款流程（付款成功、延遲、重送、晚到的款項、TTL 序列、開關兩個方向）是切片 6 的 `payment-flow.sh`，見下。
- **payment-flow.sh（切片 6，`task helm-kind` 內、`payment.sh` 之後）**：把 release 升級成 `deployments/helm/kind/payment-flow-values.yaml`（`ordering.require_payment` 開、`auth.mode` required 與兩把靜態 key `shopper`／`admin`、`paymentRpc.config.mock_control` 開）後走完整付款流程，**結束時一律還原成 `kind/values.yaml`**。`mock_control` 只在這個 overlay 開；chart 預設是 `false`。九段，每段用真的 CronJob／webhook／rpc，不改 DB 以外的任何東西（只在需要「時間過去」時 `UPDATE` 到期時間與預留時間戳記，跨 process 沒有 fake clock）：①建單→預留→`CONFIRMED`→`StartPayment`→`mock pay --deliver`→`COMPLETED`，手動 `CompleteOrder` 被 `order.payment_required` 擋；②已付款但 webhook 遲到（訂單等著、付款單 `PENDING`，之後才到），以及 webhook 完全不來（`payment-expire` Job 補查發現，T6）；③同一通知重送三次（一列 `webhook_events`）；④未知事件型別與沒人擁有的 checkout 的通知；⑤顧客付款途中取消、錢晚到（T7→T8→T10，退款一次、訂單維持 `CANCELLED`）；⑥窗口到期（T5，`PaymentExpired` 取消訂單）、錢晚到（退款一次）；⑦預留 TTL（`worker expire` Job）→ `OrderCancelled` → T7 在 provider 不通（`mock fail on`）時失敗不擋 → 補查也問不到、什麼都不動、退款數 0 → `mock fail off` → T8 → 退款一次；⑧開關：`helm upgrade` 開→關→開，快照 `true` 的單不受影響（`CompleteOrder` 仍被擋、`StartPayment` 回 `payment.disabled`、已開 checkout 的單付款後 `COMPLETED`、沒開的單逾時被取消），關時確認的單快照 `false` 且可手動完成，打開後仍可手動完成並且 `StartPayment` 回 `payment.disabled`；⑨`serve`／`consume` 縮到 0：`CreateOrder` 照常到 `CONFIRMED`、`StartPayment` 回 503 `payment.unavailable`，恢復後同樣的 rpc pod（名稱與重啟次數不變）付款到 `COMPLETED`。**沒有證明的**：真 provider 的 hosted checkout 頁（mock 的 URL 是不存在的主機 `mock-pay.invalid`，Console 的新分頁只斷言位址，見 `docs/payment.md` §9 Console 真流程）、款項早於 `OrderCancelled` 到（T3→T10）與付款單尚未建出時 `OrderCancelled` 先到（T14／T15）的交錯（kind 排不出確定的順序，那兩個是 S3A 的 Postgres 競態測試）、多副本 `consume`。本機要跑時**不要用別人的 cluster**：`task helm-kind-up` 的 cluster 名稱寫死 `x-monorepo`，另開一個 cluster 要自己寫 `KUBECONFIG` 隔離的 bring-up，再直接呼叫 `payment-flow.sh <ns> <release> <chart> <values> <flow-values>`。

- **kind 失敗時留下什麼（#416、#420）**：`payment-flow.sh` 只要不是跑到最後（`fail`、`set -e` 中止、TERM／INT／HUP）就先跑 `deployments/helm/kind/diag.sh <ns> <本次開始時間>`，**之後**才還原 release——還原的 `helm upgrade` 會換掉 pod，連同 pod 的 log。診斷掛在 EXIT trap，不掛在 `fail` 上（步驟的 `fail` 多半在 `$(…)` 子 shell 裡，子 shell 不跑父 shell 的 EXIT trap）。訊號：`helm upgrade --wait`、`kubectl wait`、`rollout status` 以背景程序啟動再 `wait`（bash 只在前景 child 結束後才執行 trap），收到訊號就停掉它與它的子程序，exit 狀態是 128＋訊號（TERM 143、INT 130、HUP 129），不會被診斷或還原的狀態蓋掉。**產物**：完整內容寫成檔案，在 `$RUNNER_TEMP/kind-diag`（可用 `DIAG_DIR` 改；CI 的 `helm-kind` job 失敗時以 `kind-diag` artifact 上傳，保留 3 天，內容是 pod 的 log，所以保留期短）：`pods.txt`、`events.txt`、每個 pod 自開始時間起的完整 log `log-<pod>.txt`（`--timestamps`，每 pod 至多 `DIAG_POD_BYTES`＝5 MB）、有前一個 container 的 `log-<pod>.previous.txt`、`postgres.txt`。**Actions console** 只印位置、每個 pod 最後 `DIAG_CONSOLE_LINES`（100）行**且至多 `DIAG_POD_CONSOLE_BYTES`（8 KiB）**與它的行數／byte 數、`pods.txt`（至多 16 KiB）、`events.txt` 最後 30 行（至多 8 KiB）、`postgres.txt`（至多 32 KiB）。**上限是 byte，不是行數，而且 collector 根本碰不到 console**：`diag.sh` 其實是兩個程式。外層 wrapper 最先跑、是真實 stdout／stderr 的唯一持有者，它啟動 collector（同一個檔案，`--diag-child`），並在啟動時就把 collector 的 stdout／stderr 指向一個擷取檔、stdin 指向 `/dev/null`。所以 collector 與它啟動的任何程序都沒有指向 console 的 descriptor（沒有 fd 3、沒有 FIFO、沒有常駐 writer、沒有要等的 pid），不管腳本內哪一行 `echo`／`printf`／`::group::` 標題（含 `since` 與 `DIAG_DIR` 的內容）或子指令輸出，只會進擷取檔。collector 結束、或 wrapper 收到 TERM／INT／HUP 時，wrapper 印出擷取檔的前 `DIAG_CONSOLE_BYTES`（預設 256 KiB）byte，超過就**只印一次**說明（指出目錄，目錄名至多 200 字元），把整份擷取複製到 `$DIAG_DIR/console.full`，以 collector 的狀態碼結束（訊號則是 143／130／129）；收到訊號時先停掉 collector 底下整棵子樹（明確 pid、由內而外），再請 collector 自己結束，5 秒後還在就 KILL。呼叫端用 pipe 捕捉輸出時，wrapper 一結束 pipe 就關了：沒有任何東西在 diag.sh 結束後還握著它（D6P）。**代價**：console 不再即時串流，只在結束時一次輸出（整個收集最長 120 秒的 deadline）。`emit`（整次執行的 byte 預算 `DIAG_CONSOLE_BYTES` 減 4 KiB 保留給固定的標記與說明、各部分的單次上限）只負責決定擷取檔裡哪些內容值得那些 byte，並註明被截斷與完整內容在哪個檔案；`emit` 要在 collector 的主 shell 執行（用 `< file`，不能 `cmd | emit`，管線的子 shell 會讓預算加不起來）。console 實測：預算 65536 byte 時 1.9 KB–65.9 KB（`since` 或 `DIAG_DIR` 長達 100000 字元、30 個 pod 各一行 1 MiB、`pods wide` 5000 列、100000 列 Postgres 輸出、每個部分都不超過單次上限但加總超過預算，皆在預算內）。**上限**：整個收集在 `DIAG_DEADLINE`（120 秒）停止並明說，每次 kubectl 的逾時不超過剩餘時間，逾時後照常還原；任何一段失敗只印一行說明、整體 exit 0。**SQL 原文不收**：`postgres.txt` 只有結構化欄位——pid、`application_name`、state、wait event、交易／查詢已經多少秒、`blocked_by`（`pg_blocking_pids`）、每個鎖的 mode／granted／relation 名稱、連線數對 `max_connections`——**不選 `pg_stat_activity.query`**（那是 client 送來的 SQL 原文，literal 也在裡面）。Postgres server 的 log 隨 pod log 原樣收（不過濾：過濾文字通道會漏掉下一種格式），所以改由 `kind/deps.yaml` 的旗標讓 server 不寫 SQL 原文：沒有 statement log；`log_min_error_statement=panic`（lock wait 是 LOG 層級，比 ERROR 高，預設會附一行 `STATEMENT:` 帶出整句查詢）；`log_min_messages=log`（ERROR 的 DETAIL 會寫值，例如唯一鍵衝突的 `Key (col)=(value)`）；保留 `log_lock_waits`（等鎖超過 1 秒記下誰持有）與 `log_checkpoints`。**取捨**：不再記慢 statement（`log_min_duration_statement`），也不再有 ERROR 層級的 server log；要知道哪條 SQL 卡住，靠 `postgres.txt` 的 wait event／relation 與 lock wait 的持有者 pid 對照，不靠語句本文。測試：`kind/diag-test.sh`（`task helm-check`；D7 需要 docker 或 podman 起一個真的 Postgres，CI 的 `helm` job 有）：D1／D2 檔案與部分失敗、D3 一般失敗、D4 manifest 旗標、D5 對真入口 `payment-flow.sh` 在 helm upgrade 等待中送 TERM／INT（先診斷後還原、exit 143／130、等待中的 helm 被停掉、session 內沒有殘留程序）、D6 30 個 pod × 5000 行（console 3000 多行、30 個檔案完整）與逾時；D6L／D6W／D6E／D6S／D6M 以 byte 為單位各一格（30 個 pod 各一行 1 MiB、`pods wide` 5000 列、30 行各 100 KiB 的 events、100000 列 Postgres 輸出、30 個 pod 各自不超過單次上限但加總超過預算），console 都在預算內並註明被截斷，檔案完整、D7 真 Postgres 以合成憑證當 SQL literal 執行並持鎖：先用會洩漏的旗標證明那個字串真的在 `pg_stat_activity` 與 server log 與唯一鍵衝突的 DETAIL 裡，再證明 diag 的輸出與檔案沒有它、manifest 旗標下的 server log 也沒有它，且 server log 仍有 `still waiting for … Process holding the lock`。每格都對應一個注入故障會變紅的變異（把 `query` 欄位加回去、不 trap TERM、不停 child、各個 console 出口繞過 `emit` 直接輸出、`emit` 放進管線、總預算不扣除、不設單次上限、collector 直接拿真實 stdout／stderr、wrapper 不截斷、wrapper 替 collector 留著 console 的 descriptor（D6F 的 inode 掃描）、不 trap TERM、留下 5 秒保險計時器（D6T）、拿掉 deadline、少一個旗標、先還原再診斷、吞掉 exit 狀態）。**範圍**：只有 `payment-flow.sh` 接了 `diag.sh`；`smoke.sh`、`payment.sh`、`negative.sh`、`outage.sh`、`scale.sh` 失敗時沒有；CI 的 `if: failure()` 還原後 `kubectl logs --tail=200` 步驟仍在。訊號只在 `payment-flow.sh` 自己等待 helm／kubectl 時即時處理：訊號若落在 `$(…)` 子 shell 跑的步驟（建單、輪詢，各自最多約一分鐘）期間，父 shell 要等該子 shell 結束才處理，這一格沒有測。**實測與沒有證明的**：`diag.sh` 對一個健康的真 kind 叢集（本機、15 個 pod、一個真 Postgres）跑過一次，2 秒、16 個檔案、sessions／locks／連線數都有輸出；**沒有**在真叢集上讓 `payment-flow.sh` 失敗再觸發它（整段 flow 的失敗收集只對假 kubectl 驗過）；#416 那次 `StartPayment` 卡 3 秒的原因；artifact 上傳步驟（要等一次 `helm-kind` 失敗才跑到）。

**Rollback 順序**：全域 `task migrate -- down` 先退 payment（反向註冊順序）的最新一步 `0004`（只 DROP `payment.mock_faults`）；再一步 `0003`（DROP mock provider 的兩張狀態表）；再一步才是 `0002`（DROP 事件表與 `webhook_events`）。退版前先停 `consume`／`expire`／`serve` **與 `worker`**（ordering 讀 payment 的事件表，見下），否則通知與事件會寫進一個正在消失的表。

**ordering 側（切片 3B，`apps/worker`）**：

- **訂閱**：worker 的 ordering 多訂閱 `payment.v1.PaymentCaptured`／`PaymentExpired`（consumer group 同樣是 `<worker.consumer_group>.ordering`，cursor 放在 payment 的 `events_v1_payment{captured,expired}_offsets`）。怎麼處理只看**訂單自己的 `payment_required` 快照與狀態**（`docs/payment.md` §8），不看目前的 `ordering.require_payment`：快照 `true` 的 `CONFIRMED` 單，`PaymentCaptured` → `PAID` 再 `COMPLETED`（同一個 transaction，發 `OrderPaid`、`OrderCompleted`），`PaymentExpired` → `CANCELLED`；快照 `false` 的單一律不動；已 `CANCELLED` 的單收到 `PaymentCaptured` 被拒（log 一行 `event rejected by domain … order.cannot_pay`，ack 不重試，**錢由 payment 側 T10 退**）；已 `COMPLETED`／`PAID` 的單收到 `PaymentExpired` 被拒（`order.cannot_cancel`）。
- **指標與告警**：`ordering_payment_event_total{event="captured|expired",effect="applied|ignored|noop|unexpected"}`（被拒的事件不計在這裡，在 `usecase_duration_seconds{usecase="ordering.apply_payment_*"}` 的 kind 裡）。**`effect="unexpected"` 應該永遠是 0**：它是「快照 `false` 的 `CONFIRMED` 單收到 `PaymentCaptured`」——`StartPayment` 會擋下這種單，出現就代表上游有缺陷（開關在 `CONFIRMED` 與付款之間被改？payment 的 `consume` 為不該付款的單建了付款單？）。條件 `sum(increase(ordering_payment_event_total{effect="unexpected"}[1h])) > 0`；Grafana 規則隨切片 5 的 payment 告警一起進 provisioning，在那之前這條只能手動查。
- **poison／prune／gauge 涵蓋 payment**：`task poison -- list [payment]`、`task prune -- --older-than 720h [--dry-run] payment`、admin RPC 與 console 都認 `payment`（schema 名取自 payment 的 contract 常數，worker 與 rpc 沒有載入 payment 的 domain／provider）；worker 的 `eventbus_outbox_*`、`eventbus_inbox_*`、`eventbus_poison_messages` 多出 payment 的 5 張 outbox 表、`payment.inbox`、`payment.events_poison`，所以既有的 poison 與 retention 告警自動涵蓋 payment。`payment.webhook_events` 仍不歸 prune（上面）。
- **升級順序**：先 `migrate up`（payment 的 `0002` 是 3A 的），再換 worker。**payment 的表不存在時**（worker 先於 migrate），worker 在消費任何訊息之前以非 0 結束，訊息是 `worker: subscribed topics have no table: payment.events_v1_paymentcaptured, … — their bounded context's migrations were not applied to this database; run the migrate step … before starting this worker`（postgres 與 redis broker 都一樣，因為 redis 的 forwarder 也讀這些表）；補上 migrate 後重啟即可。刻意不做成「略過這兩個訂閱繼續跑」：那會讓付款事件沒人消費卻看不出來。
- **連線池下限 11 → 15**：多了 2 個 subscriber（`2 × 7 + 1`）。`postgres.max_open_conns` 明確設在 11–14 的部署，升級後 worker 會拒絕啟動並說明（預設 25 與 `0`＝不設上限不受影響）。
- **Rollback**：(a) 只退 worker image：舊 worker 不訂閱 payment 的事件，`PaymentCaptured`／`PaymentExpired` 留在 payment 的 outbox 表裡，ordering group 的 cursor 列不動；換回 3B 的 worker 後從 cursor 接著消費（訂單狀態決定結果：期間已取消的單收到 `PaymentCaptured` 會被拒，錢由 payment 退）。**舊 worker 在位時不要對 payment 跑 `worker prune --unconsumed`**：沒有 cursor 的表會被當成沒人要而刪掉。付款成功的單在換回之前停在 `CONFIRMED`、不會 `COMPLETED`。(b) 退 payment 的 migration 退到 `0002` 以下（DROP 事件表；`0004`、`0003` 只丟 mock 的 fault 表與狀態表，見上）：**先停 worker**。沒停的話 worker 不會結束也不會拖垮別的 BC，但那兩個 subscriber 每秒各記一行 ERROR（`Error querying for message … relation … does not exist`），直到表回來或 worker 重啟；`SIGTERM` 照常乾淨結束（`TestWorker_KeepsRunningWhenPaymentTablesVanish`）。`migrate up` 重建的是空表與新 cursor，退版期間的事件不會回來。

**Rollback（只退 payment `0004`）**：**不要用 `task migrate -- down`**——它對每個 BC 各退一步（ordering、inventory、identity、catalog、payment 的最新 migration 都會回滾，catalog 會 `DROP TABLE catalog.products`（連同資料））。只退 payment `0004`，在 payment 的資料庫執行；`0004` 只有 `payment.mock_faults` 一張表，沒有別的表依賴它，服務運行中可以退：

```sql
BEGIN;
DROP TABLE IF EXISTS payment.mock_faults;
UPDATE payment.schema_migrations SET version = 3, dirty = false;
COMMIT;
```

丟失的範圍：`payment-rpc mock fail` 設下的 fault（等於全部 `fail off`）。mock 的 checkout、付款與退款（`0003` 的兩張表）、`payment.payments`、事件表、`webhook_events` 都留著。已是 `4 dirty` 也用同一段（`IF EXISTS`）。退完之後新版 `payment-rpc` 在 `payment.mock_control` 開著時讀 `mock_faults` 會失敗（每個 provider 呼叫都回「不可用」，fail safe）：**先關 `payment.mock_control` 或退 payment-rpc 的 image**；`mock_control` 預設關，只有 kind／測試會開。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，驗證其他 BC 的版本、schema 與資料不動。要繼續退 `0003`，在 payment 回到 `3` 之後用下一段。

**Rollback（只退 payment `0003`）**（payment 在 `3` 時；已是 `4` 先照上一段退 `0004`）：**不要用 `task migrate -- down`**——它對每個 BC 各退一步（ordering、inventory、identity、catalog、payment 的最新 migration 都會回滾，catalog 會 `DROP TABLE catalog.products`（連同資料））。只退 payment `0003`，在 payment 的資料庫執行；先停 `serve`／`consume`／`expire`（或確認 `payment.provider` 不是 mock）：

```sql
BEGIN;
DROP TABLE IF EXISTS payment.mock_refunds;
DROP TABLE IF EXISTS payment.mock_checkouts;
UPDATE payment.schema_migrations SET version = 2, dirty = false;
COMMIT;
```

丟失的範圍：mock provider 記住的 checkout、付款與退款（`payment.payments.provider_ref` 指到的 ref 在 mock 眼裡變成不存在：`expire` 查詢與 `consume` 退款都回「不可用」，fail safe，付款單不會因此誤轉狀態）；`payment.payments`、事件表、`webhook_events` 都留著。只有 demo／kind 會有這些資料，真 provider 不用這兩張表。已是 `3 dirty` 也用同一段（`IF EXISTS` 讓不存在的表略過）。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，驗證其他 BC 的版本、schema 與資料不動。

## Deploy（Helm）

### Upgrade notes — #410 切片 2：`payment-consume` 改跑 payment-worker

這一版 `<release>-payment-consume` Deployment 的 image 從 `…/payment-rpc:<tag>` 換成 `…/payment-worker:<tag>`（container 名 `payment-worker`，args 只剩 `--require-durable`）。**沒變的**：Deployment 名稱與 selector labels、`<release>-payment-rpc` ConfigMap 與 `<release>-payment-rpc-env` Secret、extraEnv 的限制、resources、`terminationGracePeriodSeconds`、沒有 autoscaling 與 probe。因為 ConfigMap 沒變，`worker.consumer_group` 仍是 `payment-rpc`，consumer group 仍是 **`payment-rpc.payment`**：所以這個**名字**不變；`apps/payment-worker` 的 `TestRun_ConsumerGroupIsPinned` 從 offsets 表量到它，`deployments/helm/check.py` 檢查 chart 渲染出的這個 Deployment（image 是 payment-worker、workload 的安全形狀）。

**不再支援、不再測試：從 ≤ v0.63.3（`payment-rpc consume`）直接升到這一版。**（#446）原本 `task helm-kind` 有一步（`payment-upgrade.sh`）從 git tag `v0.63.3` 的 chart 與它發布的 ghcr image 裝起，演練「cursor 不倒退、不重放、不跳過滾動期間發布的事件、每張單恰好一張付款單」；它佔 CI 約 84 秒，已退役。現在沒有任何測試證明這條直升路徑保有那些性質，所以本文件**不**承諾：升級時的 consumer group 不新開 cursor、不依 `worker.start_from` 重放或跳過、每張 `OrderConfirmed` 恰好處理一次。**≤ v0.63.3 的部署請先升到 v0.66.0（chart 0.3.64，最後一個 CI 仍跑這個演練的版本）、確認 `payment-consume` 正常消費，再升到之後的版本。**

| 項目 | 舊 → 新 |
|---|---|
| Deployment 名稱／selector／component label | `<release>-payment-consume` ／ `payment-consume` → 不變 |
| image | `…/payment-rpc:<tag>` → `…/payment-worker:<tag>`（同 registry、同 `image.tag` 或 `appVersion`） |
| container 名 | `payment-rpc` → `payment-worker` |
| args | `consume --require-durable` → `--require-durable` |
| ConfigMap／Secret／existingSecret／extraEnv 規則 | 不變（同一份 `<release>-payment-rpc` ConfigMap 與 Secret，同樣只給 `APP_POSTGRES_URL` 與 `APP_PAYMENT_MOCK_WEBHOOK_SECRET`） |
| resources／grace／replicas／autoscaling／probe | 不變 |
| consumer group | `payment-rpc.payment` → 不變 |
| 指標 | `service.name`（Prometheus `job`）`payment-rpc` → `payment-worker`；`eventbus_*{consumer_group=~"payment-rpc\\..*"}` 不變；dashboard 的 `payment_alerts_total` 與 `eventbus_poison_messages` 改選 `job=~"payment-rpc\|payment-worker"`（`check-observability.py` 守住）。你自己寫的、選 `job="payment-rpc"` 的查詢或告警要一起改 |
| NetworkPolicy／ServiceMonitor／KEDA／HPA | NetworkPolicy（`networkPolicy.enabled` 時，沒有 ingress 規則）照舊用同一個 component label 選 pod，不變；consume 本來就沒有 ServiceMonitor（沒有 listener），也不被 HPA／KEDA 管（`check.py` 斷言沒有 HPA／ScaledObject），所以沒有要改的入口 |

- **前提**：chart 的 `appVersion`（或 `image.tag`）必須 ≥ 0.63.3（`payment-worker` 第一個發布的版本；`0.63.3` 匿名拉取實測 200），且 `ghcr.io/blackhorseya/x-monorepo/payment-worker` 是 public。`check.py` 檢查 `appVersion`。
- **手動跑 `payment-rpc consume` 的部署**（沒用這個 chart 的 Deployment、compose、systemd…）要改成 payment-worker：同一份設定檔、同樣的環境變數、同樣的 `--require-durable`，只是 binary／image 換成 `payment-worker`（沒有子命令）。舊入口（本 PR 合併後發布的第一個 `payment-rpc` image 起）以非零 exit 結束並印 `payment-rpc: consume was removed (#410): run the payment-worker binary or image instead …`，不會靜默退出。不要同時留著舊 consumer 與新 consumer 在不同的 group 名下。
- **滾動期間**：Deployment 預設 RollingUpdate，新舊 pod 短暫並存，兩邊同一個 group 分工，handler 對同一個 message id 有 inbox 冪等；沒有 consumer 的空窗（例如先 scale 到 0）事件留在 outbox 等著。
- **版本界線**：以 chart `version` 區分——**已發布的 chart（≤ 0.3.58）的 `payment-consume` 跑 `payment-rpc consume`；本 PR 把版本訂為 0.3.59，從 0.3.59 起跑 `payment-worker`**（0.3.59 要等本 PR 合併、release 之後才存在）。同一個 chart 版本號不會對應兩種內容：`deployments/helm/check-chart-bump.sh`（CI 的 `helm` job，pull request 上）比對 base 與 head 的 `templates/` 與 `values*.yaml`，有變就要求 `Chart.yaml` 的 `version` 嚴格遞增（數字比較，0.3.10 > 0.3.9；ci／ci-negative／README／只動 appVersion 的 release bot PR 不算）。
- **退版**：退到 ≤ 0.3.58 的 chart 時，`payment-consume` 又會跑 `payment-rpc consume`，所以 image 必須還含這個入口。**已發布的 `payment-rpc` image 到 0.63.4 為止都含它**（`v0.63.4` 的 `apps/payment-rpc/main.go` 仍接受 `consume`），包括 chart 0.3.58 預設的 `appVersion` 0.63.4：照原樣退回 chart 0.3.58 不必動 image tag。**移除入口的 image 是這個 PR 合併之後發布的第一個版本**（版本號要等它發布才知道）；從那個版本起，退回 ≤ 0.3.58 的 chart 卻留著 `image.tag` 在那個版本之後，舊 chart 的 `consume` arg 會讓 `payment-rpc` 以 `consume was removed` 結束（要連 image tag 一起退，或保留 chart ≥ 0.3.59）。這一段只描述已發布的版本與原始碼；沒有用真叢集演練過退版。

### Upgrade notes — chart 0.3.53：payment 預設開（#367 切片 6）

**先讀這一條：有一種升級會「無預警」把開關打開。** chart 0.3.50–0.3.52 的 release，如果已開 auth（`config.auth.mode` 是 `optional`／`required`）、已顯式 `paymentRpc.enabled=true` 且 `secrets.paymentRpc.*` 齊，但**從沒寫過** `config.ordering.require_payment`（舊預設 `false`），那麼用**同一份 values** `helm upgrade` 到 0.3.53，render 會過、升級會成功，`config.ordering.require_payment` 同時變成 `true`——chart 分不出「沒寫」和「預設值」，沒有任何錯誤擋它。**實測**（kind，helm 4.3.0）：0.3.51 的 release（auth required、payment-rpc 開、沒寫開關）用同一份 values 升級 → `STATUS: deployed`、ConfigMap 出現 `ordering.require_payment: true`、worker 多了 `APP_PAYMENT_UPSTREAM`；升級**之後**建的單 `payment_required=true`，`CompleteOrder` 回 `failed_precondition`（`order requires payment before it can be completed`，要走付款）；升級**之前**已 `CONFIRMED` 的單快照維持 `false`，仍可手動完成。`--reset-then-reuse-values` 同樣成功、同樣變 `true`（作者與 reviewer 各量一次）；**`--reuse-values` 不一樣**：reviewer 實測它升級成功但**開關沒有變**（新單 `payment_required=false`、手動完成 200），因為 `--reuse-values` 沿用舊 release 已算出的值。這只是 helm CLI 的這一種模式，不能外推到其他 helm 版本或 GitOps 工具。**升級前請在你的 values 顯式寫 `config.ordering.require_payment: true` 或 `false`**（寫 `false` 之前也要確認 `paymentRpc.enabled` 你要不要）。

**這個版本的預設改了什麼**：chart 預設 `paymentRpc.enabled: true` 與 `config.ordering.require_payment: true`。**binary 預設不變**（`ordering.require_payment` 預設 `false`，`task run`、compose、沒設 payment 的環境都不受影響）。`config.auth.mode` 預設仍是 `off`，而 auth 關著沒有人付得了款，所以：

- **全新安裝**只給一個資料庫與 catalog-rpc 的 key（舊 chart 需要的一切）→ **render 失敗**（`ci-negative/first-install-defaults.yaml`），訊息指名兩條出路：①讓 payment 活著：`config.auth.mode` 設 `optional`／`required`（加 key），並給 `secrets.paymentRpc.apiKey` 與 `secrets.paymentRpc.mockWebhookSecret`（或 `secrets.paymentRpc.existingSecret`）；②**明確關掉**：**兩個**值都要設——`config.ordering.require_payment=false` 與 `paymentRpc.enabled=false`（只關其中一個仍會 render 失敗）。
- **provider 目前只有 mock，沒有人付得了款**（`https://mock-pay.invalid/checkout/…` 是不存在的主機）。所以預設開著、也能 render 的部署，每張需要付款的訂單會在 `paymentRpc.config.window`（預設 30 分鐘）後被取消，除非有人用 `payment-rpc mock pay` 付款——那個入口（`paymentRpc.config.mock_control`）預設關，只給 kind 與測試。接真流量的部署在有真 provider 之前**不要開**：關掉的方法就是上面的兩個值。`helm install`／`upgrade` 結束時 NOTES.txt 第一段就是這段警告。
- 已經 `CONFIRMED` 的訂單不受影響：快照在確認的那一刻存下，之後怎麼改開關都不動它（`docs/payment.md` §8）。

**實測過的升級格子**（kind，真的 `helm upgrade`；沒列的格子沒有測，不要當成保證）：

| 起點（release） | 指令 | 結果 |
|---|---|---|
| 0.3.49，auth off，沒有 payment | `-f` 自己的 values | **render 失敗**（auth 關著＋開關預設開），訊息列兩條出路；release 維持 rev 1，pod 一個都沒動 |
| 同上 | `--reuse-values`（不帶 `-f`） | **失敗，但是 template 的 nil pointer**（`.Values.paymentRpc.enabled`）：helm 不會把新頂層 key 的預設補進 `--reuse-values`，helm 不會把新頂層 key 的預設補進 `--reuse-values` 的結果（`.Values.paymentRpc` 是 nil）；這不是這次改預設造成的（`paymentRpc` 這個頂層 key 從 0.3.50 就有了，沒有在 0.3.50 的 chart 上單獨量測）；release 不動 |
| 同上 | `--reset-then-reuse-values` | 同第一列：明確失敗、不動 |
| 同上 | `-f` 自己的 values `--set config.ordering.require_payment=false --set paymentRpc.enabled=false` | 升級成功（rev 2），沒有任何 payment 物件 |
| 0.3.51，auth required，payment-rpc 開且 secrets 齊，**沒寫**開關 | `-f` 同一份 values | **升級成功且開關變 `true`**（上面第一條；無預警的那格） |
| 同上 | `--reset-then-reuse-values` | 升級成功，開關變 `true`（同上） |
| 同上 | `--reuse-values` | 升級成功，**開關沒變**（新單 `payment_required=false`，手動完成 200；reviewer 實測，helm 4.3.0） |
| 全新安裝，只給資料庫與 catalog-rpc key | `helm install` | render 失敗，訊息列兩條出路 |
| 全新安裝，auth＋payment secrets 齊 | `helm install` | 成功；開關 `true`；`task helm-kind` 的 `payment-flow.sh` 走完整流程 |

**用 `--reuse-values` 升級的人**：從 0.3.49 之前沒有 `paymentRpc` 的 release 用 `--reuse-values` 會得到上面的 nil pointer（不是設定錯誤）。改用 `-f <你的 values>`，或 `--reset-then-reuse-values`（helm 3.14+），並照上面顯式寫開關。

**怎麼關（回到沒有 payment 的行為）**：`--set config.ordering.require_payment=false --set paymentRpc.enabled=false`（values 檔寫同樣兩個鍵）。已經在跑的 payment-rpc 在 `paymentRpc.enabled=false` 後被移除；`payment` schema 的資料與 migration 不動。

**GitOps（ArgoCD、Flux）**：它們用 `helm template` 渲染，上面「升級成功／失敗」的實測是 `helm upgrade` 對活的 release；`helm template` 不看活的 release，所以**沒有任何東西擋住**上面那個無預警的格子。用這類工具的人更需要在 values 裡顯式寫開關。

`deployments/helm/x-monorepo/` 是這些 image 的 chart，沒有 dependency、不假設 ingress controller 或 secret 工具。它做的事是把
README 的 server / Events 節講的「部署要配合的行為」寫成算式，而不是讓人手抄：

| 物件 | 怎麼接 app |
|---|---|
| ConfigMap `config.yaml` | `values.config` 原樣 render（key 同 `configs/config.example.yaml`），`APP_CONFIG` 指過去；預設 `http.docs: false`、`log.format: json`、`auth.mode: off` |
| Secret `<release>-env` | `secrets.{postgresUrl,redisUrl,apiKeys,jwtHs256Secret}` → `APP_POSTGRES_URL` / `APP_REDIS_URL` / `APP_AUTH_API_KEYS` / `APP_AUTH_JWT_HS256_SECRET`（env 蓋過檔案）；或 `secrets.existingSecret` 用自己管的 |
| rpc Deployment | liveness `/api/healthz`、readiness `/api/readyz`；`rpc.shutdown.{delaySeconds,timeoutSeconds}` 變成 `APP_HTTP_SHUTDOWN_*`，`terminationGracePeriodSeconds = delay + timeout + graceMargin`，`readiness.periodSeconds > delaySeconds` 直接 render 失敗。image 是 distroless 沒 shell，所以沒有 `preStop: sleep`——app 自己的 `shutdown_delay` 就是那個 sleep。**等 catalog 是盡力而為**：`rpc.waitForCatalog`（預設 60s）是一個 initContainer，跑同一個 rpc image 的 `rpc --wait-catalog=60s`，最多等 catalog-rpc 的 `/api/readyz`；等不到就 log WARN（`catalog is not ready, starting anyway`，只含 upstream URL、不含 key）並 exit 0，rpc 照常起來——rpc 的 readyz 不依賴 catalog，catalog 掛著時 CreateOrder／SetStock 回 `UNAVAILABLE`（`catalog.unavailable`）且不寫入，catalog 回來後**同一個 rpc pod 不用重啟**就恢復（`kind/outage.sh` 實測）。所以它只讓正常升級時 catalog 先就緒，**不是硬性依賴**，而且升級當下它看到的可能是舊版 catalog-rpc 的 pod（契約沒變，版本錯位可接受）。等待時間必須小於 `rpc.progressDeadlineSeconds`（預設 600），否則 render 失敗 |
| worker Deployment | **沒有 probe**（worker 沒 HTTP server；起不來會 exit 非 0 由 Deployment 重啟，健康看 `eventbus_*` gauge 與 alert）；`terminationGracePeriodSeconds` 預設 30，< 19 render 失敗 |
| catalog-rpc Deployment（`catalogRpc.enabled`，預設 **on**，關掉會 render 失敗） | `apps/catalog-rpc`（#320）：rpc 沒有 in-process catalog 了，每次查價與 SKU 檢查都問它，沒有它 rpc 不啟動，所以它不是選配。probes 與 shutdown 算式同 rpc（`catalogRpc.shutdown.*`、`/api/healthz`、`/api/readyz` 探 Postgres）。**用自己的 ConfigMap `<release>-catalog-rpc` 和 Secret `<release>-catalog-rpc-env`，不讀 release 共用的那一組**：它遇到 `auth.jwt`、`auth.oidc.enabled`、auth 開但沒有 `auth.api_keys` 就拒絕啟動，而共用的 config / Secret 正是 rpc 的 OIDC issuer、JWT secret、使用者 API key 所在。自己的 config 只有 `http` / `postgres` / `log` / `otel`（不帶 `otel.service_name`，否則它會用 rpc 的名字上報）和 `auth: {mode: required, admin_role}`；Secret 只有 `APP_POSTGRES_URL`（= `secrets.postgresUrl`）和 `APP_AUTH_API_KEYS=rpc=<secrets.catalogRpc.apiKey>:<config.auth.admin_role>`。要自己管 Secret：`secrets.catalogRpc.existingSecret`（含這兩個 key；與 `apiKey` 擇一；不能解析成 release 的 Secret：`secrets.existingSecret`、`<fullname>-env`、`<fullname>-migrate-env`，名字取 nameOverride／fullnameOverride 之後的結果）。**兩個方向都擋**：catalog 的 `existingSecret` 指向上述任一個 → render 失敗；反過來，共用的 `secrets.existingSecret` 取了 catalog-rpc 自己生成的 `<fullname>-catalog-rpc-env` 這個名字（rpc、worker、migrate 會讀到 catalog 的 service key，chart 還會在你的 Secret 上蓋一個同名 Secret）→ 也 render 失敗；catalog 這邊比的是**最終名稱**（自填，或生成的預設名）。這兩個 key 以 `secretKeyRef`（`optional: false`）進 container，**不用 `envFrom`**：外部 Secret 少了 `APP_POSTGRES_URL` 時 pod 會停在 `CreateContainerConfigError`（`couldn't find key APP_POSTGRES_URL in Secret …`，kind 實測），而不是退回記憶體模式、readyz 200、寫入不持久。key 在、值是空字串的情況 Helm 看不到，由 binary 擋：chart 把 `--require-durable` 寫在 container `args`（env 與 Secret 改不掉 arg），`catalog-rpc --require-durable` 在 `config.Load` 之後、依**最終生效**的設定（env 蓋過檔案）判斷，`postgres.url` 空白、`auth.mode` 不是 `required`、或 `auth.api_keys` 不是「恰好一個、名稱為 `rpc` 的 key」（多個、名稱不同——含大小寫——都拒）就以非 0 結束，錯誤訊息只含設定名稱、mode 值與 key 個數，不含 URL 或 key；最後這條把 rpc 轉發要用的隱含契約寫明，也擋下「外部 Secret 其實是 rpc 的共用 Secret、帶著 console 等使用者 key」（那些 key 否則會在 catalog-rpc 通過認證）。**它縮小範圍，不是證明**：只含 `rpc=…` 的共用 Secret 擋不住，binary 看不到 rpc 的 keys，所以 `secrets.catalogRpc.existingSecret` 仍須是 catalog-rpc 專屬的 Secret（chart 會擋下指到自己產生的那幾個，擋不到你自己管的外部 Secret 的內容）；不帶這個 flag（本機開發、測試）行為不變，未知參數一律拒絕。`deployments/helm/kind/negative.sh`（`task helm-kind` 最後一步）在 kind 實跑兩種壞 Secret：缺 `APP_POSTGRES_URL` → `CreateContainerConfigError`，`APP_POSTGRES_URL=""` → 容器起來後以非 0 結束且從不 Ready。refusal 的證據讀自 Pod status 的 `terminated.message`（chart 設 `terminationMessagePolicy: FallbackToLogsOnError`，節點回收容器 log 後仍在），讀不到才退而讀 `kubectl logs`（有界重試，最多 2 分鐘）；讀取失敗只重試、不把工具的錯誤文字當成 app 輸出（CI 曾因此紅：run 41150733073）。`kind/negative-test.sh`（`task helm-check` 跑，用假 kubectl，不需要叢集）對兩個證據來源（Pod status、`kubectl logs`）各測四格：成功、空值、失敗一次後恢復、永久失敗，另有「工具錯誤文字不當 log」「refusal 帶 service key 必須失敗」；每個案例（含失敗的）還斷言 release 最後被還原（`helm upgrade --wait`）、兩個測試 Secret 被刪、讀取有界。對 `negative.sh` 做的七種變異裡，六種被這些格子抓到；第七種（log 讀取失敗當成空字串）是**等價變異**（空字串本來就不符合 grep，行為不變），不算被抓到的（#344）。`catalogRpc.extraEnv` 不能設 `APP_AUTH_*`、`APP_POSTGRES_URL`、`APP_CONFIG`（不分大小寫，`upper` 比對），否則 render 失敗——一個 `APP_AUTH_MODE=off` 就能關掉它的認證。`secrets.existingSecret`（共用那個）**不算** catalog-rpc 的來源，因為它可能帶 JWT secret。這些在 `helm template` 就擋：缺 Postgres URL、缺 key、key 含 `, : =` 或空白、`existingSecret` 與 `apiKey` 同時設，都 render 失敗（`deployments/helm/x-monorepo/ci-negative/*.yaml`，`task helm-check` 跑 `check-negative.sh` 驗每一個）。Service `<release>-catalog-rpc:<catalogRpc.service.port>` 是 rpc 的 `APP_CATALOG_UPSTREAM`（`x-monorepo.catalogRpc.upstream`）。**rpc 的那把 key 是同一把**：Secret 多一個 `APP_CATALOG_API_KEY`（= `secrets.catalogRpc.apiKey`），rpc 以 `secretKeyRef`（`optional: false`）讀它，所以 chart 自己 render 的 Secret 兩邊永遠同值；用 `secrets.catalogRpc.existingSecret` 的人要自己讓 `APP_AUTH_API_KEYS`（`rpc=<k>:<admin role>`）與 `APP_CATALOG_API_KEY`（`<k>`）同步，兩者不同步 template 看不出來——結果是 rpc 的 catalog 呼叫全被拒、使用者看到 `catalog.unavailable`、rpc 的 log 有 `catalog service credential rejected`。缺 `APP_CATALOG_API_KEY` 則 rpc pod 起不來（`CreateContainerConfigError`，kind 實測）。`rpc.extraEnv` 不能設 `APP_CATALOG_*`（不分大小寫），render 失敗 |
| migrate Job | `pre-install,pre-upgrade` hook 跑 `migrate up`：新 code 不會先於新 schema 起來。hook 跑在 release 的 Secret 存在之前，所以 chart 另 render 一個 hook-scoped 的 `<release>-migrate-env`（跑完刪）；`existingSecret` 則直接讀它 |
| prune CronJob | `worker prune --older-than <prune.olderThan>`（預設 720h、每天一次、`concurrencyPolicy: Forbid`）；`prune.forgetGroups` 對應 `--forget-group`。alert「retention overdue」的 45d 是 720h 的 1.5×，改一個要一起改 |
| expire CronJob | `worker expire --older-than <expire.olderThan>`（預設 24h、每 15 分鐘、`Forbid`）：reservation TTL，RESERVED 超過就還庫存 + 請 ordering 取消。alert「expiry overdue」的 36h 是 24h 的 1.5×，改一個要一起改；dry run / 小量跑走 `InventoryService.ExpireReservations`（console Expire 頁，要 admin role） |
| identity bootstrap（不在 chart 裡） | 第一個 account + API key：在**操作者本機**跑一次 `worker bootstrap`，經 `kubectl port-forward` 連 release 的 DB，secret 只印在你自己的 terminal 一次（做法見下面「identity bootstrap」）；**不要在叢集裡跑**——Pod 的 stdout 會進 Pod log、也可能被 log collector 收走。config `auth.api_keys` 留作 break-glass。已有 account 就是 no-op，但不要同時跑兩個 |
| auth.oidc（`config.auth.oidc`） | 外部 OIDC 登入的 values，原樣進 ConfigMap：`enabled`（預設 `false`，關著時整段不讀）、`issuers[]`（`issuer` https、`audience`＝我們的 client id、`mapping` `bound`\|`auto_provision`、dev 才用的 `allow_insecure_issuer`）。**chart 不含 IdP**——Keycloak / Auth0 / Entra 自備；Dex 只在 kind（`deployments/helm/kind/dex.yaml`，見「Dex dev IdP」）。`auth.jwt` 也有設時，`auth.jwt.issuer` 要釘住且不得等於任一 OIDC issuer，否則 Pod 起不來。list 不能用 env 覆蓋，只能寫在 values。開著時 auth interceptor 用 verifier 驗 Bearer、再以 `(iss, sub)` 對應 account（未綁定只能 `ClaimInvite`，見 `docs/identity.md`「外部身分綁定」） |
| HPA / PDB / Ingress | rpc：CPU HPA（開了就不 render `replicas`、要有 `resources.requests.cpu`）、PDB 只在可能 >1 replica 時 render、Ingress 走 HTTP/1.1（Connect / gRPC-Web），原生 gRPC 要另外 h2c |
| console Deployment（`console.enabled`，預設 off） | `web/apps/console` 的 nginx image：chart 算出來的 env 只有 `RPC_UPSTREAM=http://<release>-rpc:<rpc.service.port>`，**沒有** `APP_CONFIG`、沒有 Secret——nginx 看不到 Postgres URL 和 API key；auth 開著且沒有 OIDC 時使用者在 console 的 Credential 對話框貼自己的 key / JWT。OIDC 登入的設定（#273）走 `console.extraEnv`：`CONSOLE_OIDC_ISSUER`、`CONSOLE_OIDC_CLIENT_ID`、`CONSOLE_API_KEY_LOGIN`（`false` 關掉 API key 入口）——entrypoint 渲染成 `/config.json`（只有這三個公開值），並把 issuer 的精確 origin 加進 CSP `connect-src`；沒設或格式不合就是 OIDC 關閉。IdP 的 client 要登記 redirect URI `https://<console host>/auth/callback`，並允許 console origin 的 CORS（見 [identity-login.md](identity-login.md) §8）；probe 打 nginx 自己的 `/healthz`（rpc 的健康是 rpc probe 的事）；同一組 `podSecurityContext`（uid 65532、read-only rootfs）+ `/tmp` emptyDir 就夠。自己的 Service / Ingress（`console.ingress`，純 HTTP/1.1） |
| ServiceMonitor（`rpc.serviceMonitor.enabled`，預設 off） | Prometheus Operator 抓 rpc Service 的 `http` port、path `/api/metrics`（gin route，auth 開著也不擋）；operator 靠 label 選 ServiceMonitor，`serviceMonitor.labels` 放它要的（kube-prometheus-stack 是 `release: <name>`）。只管 rpc——worker 沒 HTTP server，它的 metrics 只能走 `config.otel.endpoint` |
| NetworkPolicy（`networkPolicy.enabled`，預設 off） | 每個 Deployment 一張（catalog-rpc 開著才有它的）、只管 ingress（egress 不碰：DB / Redis / collector 在哪是 cluster 的事）。rpc 永遠放行同 release 的 console pod，加上 `networkPolicy.rpc.ingressFrom`（NetworkPolicyPeer list；**空 = 該 port 對任何來源開**，要收緊就列 ingress controller 與 Prometheus 的 namespace）；worker 不收任何 ingress；catalog-rpc 只收同 release 的 **rpc pod**（不收 console、ingress controller；清單永遠不空，所以不會變成「任何來源」），要讓 Prometheus 抓 ServiceMonitor 就把它的 namespace 列在 `networkPolicy.catalogRpc.ingressFrom`；console 同一套 `console.ingressFrom`；payment-rpc 開著時 `serve` 只收同 release 的 rpc pod 加 `networkPolicy.paymentRpc.ingressFrom`（同樣不會是「任何來源」；ingress 開著而列表空會 render 失敗），`consume`／`expire` 不收任何 ingress。要 CNI 真的實作 NetworkPolicy 才有效，kind 預設的 kindnet 不會，所以 helm-kind 驗不了，check.py 驗形狀 |
| worker autoscaling | replica 共用 `consumer_group` 就是分工（加 replica 是否有效取決於 broker，量測見「Worker 輪詢間隔」的「要不要調」），但 handler 是 DB-bound，CPU 低時 lag 照樣長，所以 `worker.autoscaling.metric` 預設 `lag`：KEDA `ScaledObject` 用 Prometheus trigger 查 `max(eventbus_consumer_lag{consumer_group=~"<group>\\..*"})`（gauge 是 per replica，取 max 不 sum；要 `config.otel.endpoint`，否則拒絕 render）；沒 KEDA 的 cluster 用 `metric: cpu` 出普通 HPA。實測踩到的三件事都做成 render 時就擋：`lag.prometheusAddress` 要 `http://<svc>.<ns>.svc:<port>`（KEDA operator 從**它自己的** namespace 解析，裸 Service 名不通）；`minReplicas` 必須 ≥ 1（lag gauge 是 worker 自己報的，0 個 replica 就沒人報）；`pollingInterval` / `cooldownPeriod` 只對 scale-to-zero 有效，縮回去的節奏是 HPA 的 `behavior.scaleDown`（預設 stabilization 300s，`worker.autoscaling.behavior` 透傳） |

**升級到「rpc 經 catalog-rpc 查 catalog」這一版（#320 第 4 片，破壞性）**：rpc 不再 in-process 載入 catalog，chart 依此做了幾個你要照做的變動，否則 `helm upgrade` 在 render 階段就失敗（不會帶著半套設定上線）：

1. **`secrets.catalogRpc.apiKey` 必填**（或 `secrets.catalogRpc.existingSecret`）：rpc 與 catalog-rpc 共用這一把（`rpc=<k>:<admin role>` 與 `APP_CATALOG_API_KEY`）。沒設就 render 失敗，訊息點名 `secrets.catalogRpc.apiKey`。
2. **有用 `secrets.catalogRpc.existingSecret` 的人**：自己管的 Secret 要多加 `APP_CATALOG_API_KEY`（值與 `APP_AUTH_API_KEYS` 裡 `rpc=` 那把相同）；少了它 rpc 的 pod 起不來（`CreateContainerConfigError`），不會悄悄降級。
3. **需要 Postgres**：rpc 沒有 in-process catalog 了，而 catalog-rpc 沒有 `secrets.postgresUrl`（或自己的 existingSecret）就不 render。原本「不接資料庫」的 smoke 安裝（舊的 `ci/no-db-values.yaml`）不再 render（改成 `ci-negative/rpc-without-database.yaml`，要求失敗並點名 `secrets.postgresUrl`）。
4. **`catalogRpc.enabled: false` 不再可用**（render 失敗）。
5. **順序**：schema 仍由 `migrate up` hook 先跑；rpc 的 initContainer 盡力等 catalog-rpc 就緒（見上表，不是硬性依賴）。第一次升級時 catalog-rpc 與 rpc 同時被建立，rpc 會等到 catalog-rpc 就緒或逾時（預設 60s）才起來。
6. **本機開發**：`task run` 之前先 `task catalog-rpc`（`:8081`）。`task catalog-rpc` 和 `task run` 同形狀：build 之後以 `APP_CONFIG` 載入自己的設定檔 `configs/catalog-rpc.yaml`（gitignored，第一次由 `task catalog-config` 從 `configs/catalog-rpc.example.yaml` 複製；`CATALOG_CONFIG=<file>` 換檔，路徑可含空白或引號），**不讀** `configs/local.yaml`（OIDC、JWT、使用者 key 都在那邊，catalog-rpc 會拒絕或不該看到），比照 Helm 給它專屬 ConfigMap。埠（`http.addr`）、認證（`auth.mode: required` 與唯一一把開發用 key `rpc=…:admin`）、`postgres.url`（compose 那個）、`log`、`otel`（`127.0.0.1:4317` + `insecure: true`，即 `task compose-up` 的 otel-lgtm，Tempo／Loki 看得到 `catalog-rpc`）全在這份檔案。那把 key 和 `task run` 送給 rpc 的 `APP_CATALOG_API_KEY`（Taskfile 的 `CATALOG_DEV_KEY`）是同一把，埠和 `task run` 的 `CATALOG_PORT` 也要一致，`apps/catalog-rpc/config_example_test.go` 把 example 對 Taskfile 釘住。要關 OTLP 就把檔案的 `otel.endpoint` 留空（log 不會有 `otel exporting`）；collector 沒開時 catalog-rpc 照常起來、`/api/readyz` 200。檔案放 `auth.oidc` 或 `auth.jwt` 會拒絕啟動並指出該設定。和 `task run` 一樣，環境變數照一般規則 env > 檔案 > 預設，沒有額外保證——例如 shell 裡匯出的 `APP_CONFIG` 或 `APP_POSTGRES_URL` 會蓋過這份檔案。rpc 的設定範本多了 `catalog.upstream`（`docs/config.md`）。k6 腳本（`tests/k6/*.sh`）與 Console E2E（`playwright.config.ts`）都已各自起 catalog-rpc。

**migration 的鎖與前置查詢（inventory `0009`，SKU 1–64 bytes）**：`0009_stocks_sku_length` 對 `inventory.stocks` 加 `CHECK (octet_length(sku) BETWEEN 1 AND 64)`，單一 `ADD CONSTRAINT`（沒有 `NOT VALID` / `VALIDATE`：migration 在**一個交易**裡跑，取得的 ACCESS EXCLUSIVE lock 本來就持有到 commit，拆成兩句不會減少持鎖）。`ALTER TABLE` 取 **ACCESS EXCLUSIVE lock 並持有到整個 migration commit**，驗證所有既有列的整表掃描也在這把鎖底下：`inventory.stocks` 的**讀與寫**（GetStock / ListStocks / SetStock / reserve / release / commit / expire）都會等到 migration 結束，不只是寫入。stocks 每個 SKU 一筆，表預期很小，掃描是短暫的；列數很大才需要排維護窗口（與下面 ordering `0008` 同理，窗口要涵蓋讀流量）。順序：先 migrate 再升 rpc（chart 的 `pre-upgrade` hook 已保證）。

**部署前**先在 inventory 的資料庫查有沒有超長（或空）的 SKU，**這是必要步驟**；有列就不要啟動 migrate：

```sql
SELECT sku, octet_length(sku) AS bytes, on_hand, reserved FROM inventory.stocks
WHERE octet_length(sku) NOT BETWEEN 1 AND 64;
```

沒有列 = migration 直接過。這個查詢與 migration 之間仍有 race：查完到 migrate 取得鎖之前，若有人用舊版 rpc 寫進超長 SKU，migration 還是會失敗；所以查詢要緊接在 migrate 之前，且 rpc 先升級到會擋超長 SKU 的版本（proto `max_len` 與 `stock.New` 一起擋）或先擋掉 `SetStock`。

**查過不等於不會 dirty，dirty recovery 永遠要備好**：查詢回 0 筆之後，仍可能有寫入在 migrate 取得鎖之前插入超長 SKU（上面的 race）；略過前置查詢直接跑 migrate 當然也一樣。遇到時 `ADD CONSTRAINT` 報 `violates check constraint`，語句是原子的所以**不會留下 constraint**，但 golang-migrate 在執行前就已把 `inventory.schema_migrations` 標成 `version=9, dirty=true`，之後每次 `migrate up` 都拒絕（`Dirty database version 9`）。處理（版本卡在 9 dirty、沒有 CHECK）：
1. 這些 SKU 本來就不能下單（catalog 不收 > 64 bytes），先確認 `reserved = 0`，且沒有 RESERVED 的 reservation 指著它：`SELECT l.order_id, l.sku FROM inventory.reservation_lines l JOIN inventory.reservations r USING (order_id) WHERE r.status = 'RESERVED' AND octet_length(l.sku) NOT BETWEEN 1 AND 64;`；有列就先讓那些訂單走完（COMPLETED / CANCELLED / EXPIRED），不要直接刪庫存。
2. 刪除或改名那些列（`DELETE FROM inventory.stocks WHERE octet_length(sku) NOT BETWEEN 1 AND 64`，刪之前把查詢結果存起來）。
3. 重設版本再重跑：`UPDATE inventory.schema_migrations SET version = 8, dirty = false;`，然後 `task migrate -- up`。這個 reset 只適用於上面這種「`0009` 失敗後 dirty」的狀態，不是任意 dirty 版本的通用解（只是不想升級而要退掉時，直接用下面的 Rollback，同一段 SQL 在這個狀態也成立）。

**Rollback（只退 inventory `0009`）**：**不要用 `task migrate -- down`**——它對每個 BC 各退一步（反向註冊順序：payment、catalog、identity、inventory、ordering；payment 的最新一步 `0004` 會 DROP 掉 mock provider 的 fault 表 `payment.mock_faults`（連同資料）），會 `DROP TABLE catalog.products`（連同資料）並回滾 identity、ordering 的最新 migration，不是只移除這個 CHECK。只退 inventory `0009`，在 inventory 的資料庫執行。兩種狀態用同一段 SQL：9 clean（CHECK 存在）會移除 CHECK 並退回 8；9 dirty（`0009` 失敗，沒有 CHECK）`IF EXISTS` 讓 DROP 略過，只把版本設回 8 clean（超長的列仍在，要再 migrate 先照上面第 1–2 步處理）：

```sql
BEGIN;
ALTER TABLE inventory.stocks DROP CONSTRAINT IF EXISTS stocks_sku_length;
UPDATE inventory.schema_migrations SET version = 8, dirty = false;
COMMIT;
```

這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，兩種狀態（9 clean、9 dirty 且無 CHECK）都驗證 catalog / identity / ordering 的版本、schema 與資料不動。

**部署前查孤兒庫存（`SetStock` 要先有 product，#326）**：`SetStock` 只替 catalog 裡有的商品記庫存。SKU 沒有 product → `INVALID_ARGUMENT` + `stock.unknown_sku`，不寫入；catalog 沒回答（任何非 `NOT_FOUND` 的失敗）→ `UNAVAILABLE` + `catalog.unavailable`，不寫入（fail closed，不是「查不到就放行」）；已下架的 product 照常 `SetStock`（下架後仍要能盤點、出清）。檢查在 transaction 之外，等 catalog 時不佔 DB 連線。

不做 migration、不刪資料：升級前就存在、但 catalog 沒有 product 的庫存（孤兒庫存）還讀得到（`GetStock`、`ListStocks`），reserve / release / commit / expire 照舊，只是不能再 `SetStock`，要先補 product。catalog 故障時 `GetStock` / `ListStocks` 也不受影響。升級前在 catalog 與 inventory 同一個資料庫裡查（跨 schema 不能設 FK，所以這是唯一的對帳方式）：

```sql
SELECT s.sku, s.on_hand, s.reserved FROM inventory.stocks s
LEFT JOIN catalog.products p ON p.sku = s.sku COLLATE "C"
WHERE p.sku IS NULL
ORDER BY s.sku;
```

沒有列 = 沒有孤兒庫存。有列的，對每個還要賣的 SKU 先 `UpsertProduct`（admin role）補 product，之後 `SetStock` 照常；不賣的就留著，它們不影響其他 SKU。這段 SQL 由 `apps/rpc` 的測試在真實 Postgres 上直接從本文件取出執行，不是另外抄一份。

**行為變更（順序）**：自動化要先 `UpsertProduct` 再 `SetStock`（repo 裡的 k6 setup、kind smoke、Console E2E 都已照這個順序）；先 `SetStock` 的舊順序升級後會得到 `stock.unknown_sku`。`UpsertProduct` 與 `SetStock` 都要 admin role，auth 開著時 setup 兩步都用 admin key。

**migration 的鎖（ordering `0008`，`CreateOrder` 冪等）**：`0008_order_idempotency` 對 `ordering.orders` 加兩個 nullable 欄位（不重寫表、不回填），再建 partial unique index（`CREATE UNIQUE INDEX`，不是 `CONCURRENTLY`——migration 在**一個交易**裡跑）。`ALTER TABLE` 取 **ACCESS EXCLUSIVE lock 並持有到整個 migration commit**，建 index 的整表掃描也在這把鎖底下：`ordering.orders` 的**讀與寫**（GetOrder / ListOrders / CreateOrder / Cancel / Complete / Confirm）都會等到整個 migration 結束，不只是寫入；讀流量照進來會逾時。表小（本機、staging）是毫秒級，可忽略；**大表請排維護窗口，且窗口要涵蓋讀流量**（先把 rpc / worker `kubectl scale` 到 0 或擋掉流量，再讓 migrate hook 跑）。順序：先 migrate 再升 rpc（chart 的 `pre-upgrade` hook 已保證）；舊 rpc 不認識新欄位也不寫它們，新 rpc 對 NULL 的列照舊，所以 rolling upgrade 期間兩種版本可並存。回滾見下一段（只退 ordering `0008`）。

**migration 的鎖（ordering `0011`，`order_history` 的 cause，#427 切片 2）**：`0011_order_history_cause` 把 `ordering.order_history` 的 `cause` CHECK 放寬成多收 `STOCK_SHORTAGE`、`RESERVATION_EXPIRED`。migration 在**一個交易**裡跑，`ALTER TABLE` 的 ACCESS EXCLUSIVE lock 持有到 commit。這把鎖雖然落在 `order_history`，**但訂單的寫入路徑都在等它**（實測，見下）：凡是會發 order event 的 use case，都在**同一個交易**裡先 `UPDATE ordering.orders`、再 `INSERT` 一列 `order_history`——`CreateOrder`、`ConfirmOrder`（worker，`StockReserved`）、`CancelOrder`（rpc 與 worker 的 `OrderCancellationRequested`）、`CompleteOrder`、`ApplyPaymentCaptured`、`ApplyPaymentExpired`——它們卡在 INSERT；請求逾時後**整個交易回滾**（訂單、history、outbox 都沒變，重試即可）。`GetOrderHistory` 讀同一張表，也會等。只有不碰 history 的讀（`GetOrder`、`ListOrders`）不受影響。這七個 use case 逐一有測試（`pkgs/ordering` 的 `TestOrderHistory_TableLockBlocksTheWritePath`：一個 session 持有 ACCESS EXCLUSIVE，另一個 session 跑真的 use case 設短期限，斷言會等到逾時、訂單與 outbox 都回滾；鎖釋放後同一個操作成功）。

為了讓這段等待短：(1) 新 CHECK 是舊 CHECK 的超集，既有列必定滿足，所以用 `ADD CONSTRAINT … NOT VALID`，**不掃整張表**——持鎖時間不隨 history 變大（`NOT VALID` 仍對之後每一筆新增或更新的列強制檢查，有測試；沒有另外 `VALIDATE`，真要驗證另開一份 migration）。(2) migration 開頭 `SET LOCAL lock_timeout = '3s'`：拿不到鎖時 `ALTER TABLE` 會排在鎖佇列裡，**之後所有碰這張表的請求都排在它後面**，一個長交易就能讓整條訂單寫入路徑一起卡住；有 timeout 就改成 3 秒後 migration 失敗、放掉佇列。3s 小於 `http.timeout` 預設的 10s。測試：一個長交易持有表的鎖時 `up` 在 timeout 內失敗，拿掉 `lock_timeout` 這個測試就轉紅。我們沒有宣稱持鎖時間是幾毫秒，也沒有宣稱 `order_history` 一定小：它隨訂單量增長，所以靠不掃表與 timeout 這兩件事，不靠表小。

**`lock_timeout` 失敗之後**：golang-migrate 在執行檔案之前就把版本標成 `11 dirty`，所以逾時失敗會留下 `ordering 11 dirty=true`（表沒變，CHECK 還是 `0010` 的；測試驗證）。處理：等持鎖的長交易結束（或避開尖峰），用下面的 rollback 段落那段 SQL 把版本設回 `10` clean（dirty 狀態用同一段 SQL），再 `task migrate -- up`。chart 的 migrate hook 在 dirty 時會一直失敗直到重設，所以升級會被擋在 hook，不會帶著半套 schema 往下走。

**順序（升級）**：先 migrate 再升 worker／rpc／inventory（chart 的 `pre-upgrade` hook 已保證）。**新 worker 不能跑在舊 schema（`10`）上**：inventory 送來帶 `cause` 的 `OrderCancellationRequested` 時，ordering 要記 `STOCK_SHORTAGE`／`RESERVATION_EXPIRED`，而 `10` 的 CHECK 拒絕這兩個值（`apps/migrate` 的 `0011` rollback 測試在退版後直接驗證），所以那筆取消記不下來。所以不要在新 worker 還在跑的時候把 schema 退回 `10`。**舊 image 遇到 `11` 的 schema 也不能升回去**（實測，用 `15f50b5` 建出的 `migrate` 對已經是 `11` 的資料庫跑 `up`）：`migrate ordering: no migration found for version 11: read down for version 11 .: file does not exist`，資料與版本（`11 dirty=false`）都沒動。（重做：`git archive <切片 1 的 commit> go.work apps pkgs tools | tar -x -C <空目錄>`，在該目錄 `go build -o <舊> ./apps/migrate`；用目前的樹 `go build ./apps/migrate` 對一個拋棄式 Postgres 跑 `up` 到 `11`，再用 `<舊> up`。）chart 的 migrate hook 只掛 `pre-install,pre-upgrade`，所以 `helm upgrade` 回舊 chart 會在 hook 失敗，`helm rollback` 則不跑它（只換 workload，不動 schema）。舊 worker（切片 1）跑在 `11` 上沒問題：它只寫舊的六個值，而且**沒有任何 production 路徑讀 history**（`HistoryStore.List` 在切片 1 只有測試呼叫），所以 `11` 的列帶新值不會讓它報錯。`GetOrderHistory` 讀到不認得的 `cause` 時（例如更新的版本寫的值）回 `UNSPECIFIED`，不會讓整個 RPC 失敗。

**Rollback（只退 ordering `0011`）**：**不要用 `task migrate -- down`**（理由同下面各段：它對每個 BC 各退一步）。**順序：先把 worker 縮到 0（它是唯一會寫新值的程序），再執行下面的 SQL，最後才 `helm upgrade` 回切片 1 的 chart／image**（此時舊 migrate 看到的是 `10`，是 no-op；反過來先升回舊 image，hook 會因為看到 `11` 而失敗）。只退 ordering `0011`，在 ordering 的資料庫執行：

```sql
BEGIN;
SET LOCAL lock_timeout = '3s';
UPDATE ordering.order_history SET cause = 'CANCELLATION_REQUESTED' WHERE cause IN ('STOCK_SHORTAGE', 'RESERVATION_EXPIRED');
ALTER TABLE ordering.order_history DROP CONSTRAINT IF EXISTS order_history_cause_check;
ALTER TABLE ordering.order_history ADD CONSTRAINT order_history_cause_check
    CHECK (cause IN ('UNSPECIFIED', 'RPC_REQUEST', 'PAYMENT_CAPTURED', 'PAYMENT_EXPIRED', 'STOCK_RESERVED', 'CANCELLATION_REQUESTED'));
UPDATE ordering.schema_migrations SET version = 10, dirty = false;
COMMIT;
```

丟失的範圍：帶 `STOCK_SHORTAGE`／`RESERVATION_EXPIRED` 的列**改記成** `CANCELLATION_REQUESTED`（切片 1 的 ordering 對同一個事件寫的就是這個值）——列都留著，只是原因的細分資訊永久忘記；訂單、明細與 outbox 都不動。兩種狀態用同一段 SQL：11 clean 退回 10；11 dirty（`0011` 失敗，整個檔案在一個交易裡，所以 CHECK 還是 `0010` 的）只把版本設回 10 clean，之後排除原因再 `task migrate -- up`。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，驗證其他 BC 的版本、表、欄位與資料不動，並驗證退版後 CHECK 拒絕新值、接受舊值。要繼續退 `0010`，在 ordering 回到 `10` 之後用下一段。

**migration 的鎖（ordering `0010`，`order_history`，#427）**：`0010_order_history` 只有一個 `CREATE TABLE ordering.order_history`（primary key `(order_id, seq)`，CHECK 約束只看自己的列），**不碰 `ordering.orders`，也沒有指向它的 FK**——所以不會在 `orders` 上取鎖，rpc／worker 的讀寫不會等它。migration 仍在一個交易裡跑，但鎖的對象只有新表。既有訂單**不 backfill**：升級前建立的訂單沒有 history，或只有升級之後的轉換（缺前段），讀取時是空清單、不是錯誤。順序：先 migrate 再升 worker／rpc（chart 的 `pre-upgrade` hook 已保證）；舊 worker／rpc 不寫這張表，新舊版本在 rolling upgrade 期間可並存（只是舊版處理的轉換不會有 history）。`worker prune` 不碰這張表，所以它不會自己長大成維運問題之外的東西：每張訂單最多 4 列（PENDING→CONFIRMED→PAID→COMPLETED 的轉換次數上界）。

**Rollback（只退 ordering `0010`）**（ordering 在 `10` 時；已是 `11` 先照上一段退 `0011`）：**不要用 `task migrate -- down`**（理由同下面各段：它對每個 BC 各退一步）。只退 ordering `0010`，在 ordering 的資料庫執行：

```sql
BEGIN;
DROP TABLE IF EXISTS ordering.order_history;
UPDATE ordering.schema_migrations SET version = 9, dirty = false;
COMMIT;
```

丟失的範圍：整張 `ordering.order_history`（所有訂單的轉換紀錄，連同 actor 與 cause）——它是紀錄，不是 outbox，沒有別的地方能重建，退版就是永久忘記；訂單、明細與 outbox 都不動。兩種狀態用同一段 SQL（`IF EXISTS`）：10 clean 退回 9；10 dirty（`0010` 失敗，整個檔案在一個交易裡，所以表不在）只把版本設回 9 clean，之後排除原因再 `task migrate -- up`。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，驗證其他 BC 的版本、表、欄位與資料不動。要繼續退 `0009`，在 ordering 回到 `9` 之後用下一段。

**migration 的鎖（ordering `0009`，`payment_required` 快照）**：`0009_payment_required` 對 `ordering.orders` 加一個 `boolean NOT NULL DEFAULT false` 欄位（常數 DEFAULT，PostgreSQL 11 起不重寫表、不掃表），並建立 `ordering.v1.OrderPaid` 的事件表（`events_v1_orderpaid` 與 `_offsets`）。migration 在**一個交易**裡跑，`ALTER TABLE` 取 **ACCESS EXCLUSIVE lock 並持有到 commit**：`ordering.orders` 的讀與寫會等到它結束；因為沒有掃表，持鎖時間是毫秒級，但拿鎖要排在進行中的長交易後面，同樣建議在流量低的時段跑。既有訂單的 `payment_required` 全是 `false`（它們在 Payment 存在之前就已 `CONFIRMED`），所以升級後它們照舊可以直接 `CompleteOrder`。順序：先 migrate 再升 worker／rpc（chart 的 `pre-upgrade` hook 已保證）；舊 worker 不認識新欄位也不寫它，新欄位有 DEFAULT，所以 rolling upgrade 期間兩種版本可並存。`ordering.require_payment` 預設 `false`，升級本身不改變任何下單行為。

**Rollback（只退 ordering `0009`）**（ordering 在 `9` 時；已是 `10` 或 `11` 先由新到舊退：`0011`、再 `0010`）：**不要用 `task migrate -- down`**（理由同上一段：它對每個 BC 各退一步）。只退 ordering `0009`，在 ordering 的資料庫執行：

```sql
BEGIN;
DROP TABLE IF EXISTS ordering.events_v1_orderpaid_offsets;
DROP TABLE IF EXISTS ordering.events_v1_orderpaid;
ALTER TABLE ordering.orders DROP COLUMN IF EXISTS payment_required;
UPDATE ordering.schema_migrations SET version = 8, dirty = false;
COMMIT;
```

丟失的範圍：`ordering.orders` 每一列的 `payment_required`（快照隨之消失：退版前以 `ordering.require_payment: true` 進 `CONFIRMED` 的訂單，退版後又可以直接 `CompleteOrder`，所以只在 Payment 還沒上線、沒有任何訂單真的欠款時退），加上 `OrderPaid` 的事件表（切片 1 沒有發布者，表是空的）；訂單與明細本身都留著。`ALTER TABLE` 取 ACCESS EXCLUSIVE，持有到 `COMMIT`。兩種狀態用同一段 SQL（`IF EXISTS`）：9 clean（`0009` 已套用）退回 8；9 dirty（`0009` 失敗，例如 `ALTER TABLE` 在 `lock_timeout` 內拿不到鎖；整個檔案在一個交易裡，所以欄位與事件表都不在）只把版本設回 8 clean，之後排除原因再 `task migrate -- up`。這個 dirty 分支只驗證過「失敗後欄位與事件表都不在」的狀態。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，兩種狀態都驗證其他 BC 的版本、表、欄位與資料不動。要繼續退 `0008`，在 ordering 回到 `8` 之後用下一段。

**Rollback（只退 ordering `0008`）**（ordering 在 `8` 時；已是 `9`、`10` 或 `11` 先照上面各段由新到舊退：`0011`、`0010`、再 `0009`）：**不要用 `task migrate -- down`**——它對每個 BC 各退一步（反向註冊順序：payment、catalog、identity、inventory、ordering；payment 的最新一步 `0004` 會 DROP 掉 mock provider 的 fault 表 `payment.mock_faults`（連同資料）），不只退 ordering：catalog 會 `DROP TABLE catalog.products`（連同資料），identity、inventory 的最新 migration 也一併回滾。全域 `down` 的語意不變，只在明確知情要退全部 BC 時使用。只退 ordering `0008`，在 ordering 的資料庫執行：

```sql
BEGIN;
DROP INDEX IF EXISTS ordering.orders_customer_idempotency_key;
ALTER TABLE ordering.orders DROP COLUMN IF EXISTS request_hash, DROP COLUMN IF EXISTS idempotency_key;
UPDATE ordering.schema_migrations SET version = 7, dirty = false;
COMMIT;
```

丟失的範圍：`ordering.orders` 每一列的 `idempotency_key` 與 `request_hash`，加上那個 partial unique index——已記住的冪等保護隨之消失，退版後用同一個 key 重送 `CreateOrder` 會再建一張新單；訂單與明細本身都留著。`ALTER TABLE` 取 ACCESS EXCLUSIVE，持有到 `COMMIT`（與上面 up 相同）。兩種狀態用同一段 SQL（`IF EXISTS`）：8 clean（`0008` 已套用）退回 7；8 dirty（`0008` 失敗，例如 `ALTER TABLE` 在 `lock_timeout` 內拿不到鎖；整個檔案在一個交易裡，所以欄位與 index 都不在）只把版本設回 7 clean，之後排除原因再 `task migrate -- up`。這個 dirty 分支只驗證過「失敗後欄位與 index 都不在」的狀態；若 `0008` 因你手動建過的同名欄位或物件而失敗，這段 SQL 的 `DROP ... IF EXISTS` 會連那些一起刪，先確認再執行。這段 SQL 由 `apps/migrate` 的測試在真實 Postgres 上直接從本文件取出執行，兩種狀態都驗證其他 BC 的版本、表、欄位與資料不動。

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
  --set-string secrets.postgresUrl='postgres://…' \
  --set-string secrets.catalogRpc.apiKey="$(openssl rand -hex 24)" \
  --set config.otel.endpoint=otel-collector:4317   # image 是 public 的 ghcr，不用 pull secret；catalogRpc.apiKey 是 rpc 與 catalog-rpc 共用的 service key（必填，不能含 , : = 或空白）
```

`check.py` 是 chart 裡唯一有邏輯的地方的測試：rpc 的 grace period 算式與 env、readiness period 的關係、worker 沒 probe、migrate hook
與它的 Secret 的順序、prune 的 args、console 的 `RPC_UPSTREAM` 指到這個 release 的 rpc Service 且沒有 `APP_*` / Secret。ServiceMonitor 選到 rpc Service 的 port、NetworkPolicy 的 peers（console 一定在、`ingressFrom` 原樣、空就不出 `from`）。`ci/` 十個 profile（預設 + DB；redis broker + HPA + ingress + auth required；existingSecret（rpc 的共用 Secret 與 catalog-rpc 自己的各一）；console + 自己的 ingress；hardened：NetworkPolicy 列 ingress-nginx / monitoring + ServiceMonitor + console；oidc：auth required + 兩個 issuer，驗 `auth.oidc` 原樣進 ConfigMap、布林沒被 YAML 1.1 吃掉；`catalog-rpc-values`：rpc 的 OIDC / JWT / 使用者 key 全開，catalog-rpc 的 ConfigMap 與 Secret 仍不含；`catalog-rpc-existing-secret-values`：外部 Secret；`payment-rpc-values`：rpc 的 OIDC／JWT／使用者 key／Redis broker 全開、payment-rpc 三個 workload ＋ webhook ingress ＋ NetworkPolicy ＋ ServiceMonitor，驗 payment-rpc 的 ConfigMap 與 Secret 不含這些、ingress 只有 webhook path；`payment-rpc-existing-secret-values`：外部 Secret）。每個 profile 都帶 `secrets.catalogRpc.apiKey`（或 existingSecret），因為 rpc 沒有 catalog-rpc 就不啟動；原本的「無 DB」profile 不存在了，`ci-negative/` 的 100 個案例（含 `rpc-without-database.yaml`、70 個 `payment-rpc-*`／`rpc-extra-env-payment-*`／`worker-extra-env-payment-*`／`require-payment-*`）由 `check-negative.sh` 要求 `helm template` 失敗並點名原因。
在 CI 的 `helm` job 跑。真的裝一次是 `task helm-kind`（CI 的 `helm-kind` job）：起 kind cluster、`deployments/helm/kind/deps.yaml`
放一個拋棄式 Postgres + Redis（chart 沒有 subchart dependency，production 接 managed DB，這份只給 smoke 用）、從 working tree build 五個 Go image（rpc / catalog-rpc / payment-rpc / worker / migrate）+ console image（`web-build` → nginx）
`kind load` 進去、`helm upgrade --install --wait` 兩次（第一次走 pre-install hook、第二次走 pre-upgrade），每次跑 `kind/smoke.sh`：
UpsertProduct → SetStock → CreateOrder → worker 透過 Redis Streams 消費 OrderCreated → GetStock 看到 reserved；console 的 `/reservations` 回 `index.html`、`/api/readyz` 經 nginx proxy 到 rpc。本機約 1 分鐘（cluster 已存在時），`task helm-kind-down` 刪 cluster。
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

**worker 有下限**：每個 subscriber 從領到訊息到 ack 都佔著一條交易連線（watermill-sql 在那條交易裡 `FOR UPDATE` 住 cursor），handler 的 use case 又要另一條。池子小於 `2 × subscriber 數 + 1`（目前 7 個 subscriber = 15；3B 之前是 5 個 = 11）時，所有 subscriber 同時忙就會彼此卡死，handler 一個個撞 `worker.handler_timeout`、訊息進 poison。實測 `max_open_conns=1`：reservation.js 每筆都 10s 後 `handler timed out`。所以 worker 啟動時直接拒絕低於這個值的設定（`0` = 不設上限除外）。新增 BC 或 handler 時下限跟著變。

**rpc 沒有死鎖的下限，代價是延遲**：池子滿了請求排隊等連線（受 `http.timeout` 約束，這條沒實測）。本機 100 VU 打 `orders.js`（10s）：

| `max_open_conns` | server 端看到的連線數（尖峰） | 吞吐 |
|---|---|---|
| 0（不設上限） | 35 | 2745 req/s |
| 25（預設） | 25 | 2820 req/s |
| 10 | 11（多 1 條的原因沒查） | 2793 req/s |

上限住了，吞吐沒掉（這台筆電上；100 VU 時連線數不是瓶頸）。`max_idle_conns` 小於實際併發時，多出的連線每次用完就關、下次重連；看到 `pg_stat_activity` 的連線一直在重建就把它調到接近 `max_open_conns`。

**搭配 KEDA / HPA**：replica 數是動態的，所以公式用 `maxReplicas`。縮容時 pod 的池子隨 pod 關閉（SIGTERM 時 `NewDB` 的 cleanup 關池子）；`conn_max_idle_time`（5m）讓尖峰過後多開的連線自己回收，`conn_max_lifetime`（30m）讓 failover / DNS 變動後池子換到新的 backend。
