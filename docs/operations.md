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
| HTTP routes | `http_server_request_duration_seconds`（otelgin） | `/api/*` 的 route / status、p95 |
| Auth | `auth_decisions_total{result, method}`、`rpc_server_call_duration_seconds_count{rpc_response_status_code="permission_denied"}` | 每秒多少 ok / public / anonymous / missing / invalid，各用什麼 credential；被拒的三種（沒帶、帶錯、role 不夠）疊在一起看。counter，`sum` |
| Identity | `identity_resolver_lookups_total{result}`、`identity_resolver_cache_entries` | key resolver 的 lookup 各 result（hit / miss / db_ok / db_miss / error）每秒多少、cache hit ratio、error 率、每個 replica 的 cache 大小。counter `sum`，cache size 是 per replica（`max by (instance)`） |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTel runtime instrumentation 的名字，OTLP 與 `/api/metrics` 相同。`/api/metrics` 用自建 registry，沒有 client_golang 的 `go_memstats_*`、`process_*` |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

Alert rules 也從 repo provision（`deployments/docker/grafana/provisioning/alerting.yaml` → folder `x-monorepo`，每分鐘評估；group
`worker-eventbus` 全部用 `max by`，gauge 是 per replica 不能 sum；group `rpc-auth` 與 `rpc-identity` 讀 counter，跨 replica `sum`）：

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

測試：`dex-prebind.sh` 是本機開發工具，沒有自動化測試。Playwright 走真 Dex 的 OIDC E2E 等 #295。

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

## Load test（k6）

`tests/k6/` 是兩個對著真的 `rpc` 打的 k6 腳本（Connect over JSON，`lib.js` 的 `rpc()` 按 method 打 tag，`API_KEY` 有設就帶 `X-API-Key`；兩支都打 `identity.v1.IdentityService/ListAccounts`）：

| 腳本 | 打什麼 | 驗什麼 |
|---|---|---|
| `ratelimit.js` | server 開 `http.rate_limit.rps=RPS`，以 `RPS × FACTOR`（預設 4）的 constant-arrival-rate 打 ListAccounts | 被拒的每一筆都是 429 + `code: resource_exhausted` + `x-error-code: request.rate_limited` + 整數 `Retry-After ≥ 1`；放行數 ≈ `RPS × 秒數`（+ 一個 burst）、其餘全拒——limiter 是拒絕不是排隊 |
| `timeout.js` | server 開 `http.timeout=1ns`（要有 `postgres.url`：context 過期是在 DB call 才浮上來，in-memory repo 不看它） | 每一筆都是 504 + `code: deadline_exceeded` + `x-error-code: request.timeout`，不是 INTERNAL（pgx 包過的 context error 也要被對應到）；server 沒斷線 |

```bash
task k6 -- ratelimit -e RPS=50    # 對 http://127.0.0.1:8080（task run）打；BASE_URL / API_KEY / VUS / DURATION / RPS 用 env 蓋；server 要先用 APP_HTTP_RATE_LIMIT_RPS=50 APP_HTTP_RATE_LIMIT_BURST=50 起
task k6-auth-matrix               # tests/k6/auth-matrix.sh：ratelimit / timeout 在真的 rpc + Postgres 上各種 cell 跑一次，見下方「Auth 組合」；CI 的 k6-auth job 就是它
task k6-wire                      # tests/k6/wire.sh：k6 腳本打 tests/k6/wire_server.py（回壞掉的 HTTP response），驗 key 不進 k6 的輸出 / summary / metrics tag，見下方「壞掉的 HTTP response」；不需要 Postgres 和 rpc，CI 的 k6-wire job 跑它
task k6-smoke                     # tests/k6/smoke.sh：ratelimit、timeout 各自起一個 bin/rpc（:18090、:18100，local Postgres、對應的 env），rate limit 50 rps、timeout 1ns；CI 的 k6 job 就是它
```

### Auth 組合

`task k6-auth-matrix`（`tests/k6/auth-matrix.sh`）目前只剩 ratelimit / timeout 的 key 外洩 cell（11 格），真的 rpc + Postgres：API key 設成自造的假值，腳本原本的 thresholds 不動，另外要求這把 key 不出現在 k6 的輸出、`--summary-export` 與 `--out json`——包括 server 回一次 503 或 307 並把請求的 `X-API-Key` 反射回來（`tests/k6/fault_proxy.py`，test-only，不改產品行為），或 key 剛好等於腳本用來判斷的 code 時。每個 cell 有自己的 rpc 與 log，並寫明 k6 必須以哪個 exit code 結束。

### 壞掉的 HTTP response（`task k6-wire`）

上游或 proxy 回的不是合法 HTTP（status code 是 key、version 是 key、header 行沒有冒號且帶著 key）時，Go 的 parser 把「讀不懂的那段」原樣放進錯誤，而 k6 在 `http.post` 回傳之前就把它印成 `Request Failed` warning，並寫進每一筆樣本的 `error` tag——`lib.js` 的 `redact` 只包得到腳本自己印的診斷，在這之前攔不到。實測（k6 v2.2.0）：只在腳本裡包 try/catch **沒有用**（請求失敗時 `http.post` 不丟例外，warning 已經印了）。做法是三件事一起：

1. `options.throw: true`（`lib.js` 的 `throwOnError`）：請求失敗改成丟例外，k6 不再自己印 warning；
2. `rpc()` 抓住例外，只用訊息挑一個固定分類，訊息本身不印、不放進回傳：回一個 `status: 0` 的 response，`errorCode()` / `knownCode()` 給 `transport.<class>`——`malformed_response`（status / version / header / chunk 讀不懂）、`truncated_response`（`unexpected EOF`）、`timeout`、`connection_refused`、`dns`、`connection_closed`（對方直接關線）、`other`；`rpc()` 每個 VU 前三次印 `<RPC>: no response, transport.<class>`（整句過 `redact()`：決策用 `knownCode()`，輸出和 `errorCode()` 一樣遮罩，key 剛好等於 `transport.<class>` 也不會出現）。這次不是 `rpc_duration` 的樣本；
3. `systemTags` 拿掉 `error`（自由文字，就是 parser 的訊息），保留 `error_code`（k6 的固定數字分類，例如 1000 = 一般 transport 錯誤）。

`rpc()` 以外的任何 `http.*` 呼叫都不受保護：沒被抓到的例外 k6 會連訊息一起印。新腳本一律走 `rpc()`，`options` 帶 `throw: throwOnError`。

對監控查詢的影響：repo 裡沒有東西讀 k6 的 `error` tag（k6 沒接 Grafana / Prometheus remote write），所以沒有 dashboard 或 alert 受影響。自己用 `--out` 接 k6 metrics 的人，原本 `error` tag 現在沒有了，改看 `error_code`（或 `status == 0`）；細節看 log 的 `transport.<class>`。

另外 transport 失敗不再被默默放過：k6 對沒有 response 的請求不計 `http_req_failed`，所以 `timeout.js` 打到一個回壞掉 HTTP 的 server 時，原本每筆都在 `check` 丟例外、`timed_out` 沒有樣本 → `rate==1` 空過、exit 0；現在 `check` 看得到 `status 0`，`timed_out` 是 0% → exit 99。

`tests/k6/wire.sh`（`task k6-wire`，不需要 Postgres 和 rpc，約 1 分鐘）：各腳本 × 六種壞掉的回應（`tests/k6/wire_server.py`：`status` / `version` / `header` / `chunk` 帶著請求的 `X-API-Key`，`truncated` body 不完整，`closed` 連線直接關），每個 run 都要非零退出，`API_KEY`（自造的假 key）不能出現在 k6 輸出、`--summary-export`、`--out json` 的任何一行，metrics 不能有自由文字的 `error` tag，`closed`（key 根本沒上線）要有 `transport.<class>` 的診斷。每個 cell 還要證明 fixture 真的在場：server 要在 5 秒內 listen、跑完仍活著、自己印出 `served <mode>`（壞回應確實送出），而且 k6 回報的失敗分類必須恰好是該 mode 的（`status` / `version` / `header` / `chunk` → `malformed_response`、`truncated` → `truncated_response`、`closed` → `connection_closed`），出現其他分類（例如 server 缺席時的 `connection_refused`）就紅——光看 exit 非零和沒有 key 證明不了什麼。最後兩個負例 cell 讓 `wire.sh` 自己再跑一次：server 不啟動（`WIRE_SERVER_CMD=false`）、server 回錯的 mode（`WIRE_SERVE_AS=closed`），它必須失敗，否則 gate 本身標紅。失敗用 go test 的 `--- FAIL:` 格式印，`scripts/red-stage.sh` 讀得懂。

CI 的 `k6-auth` job 和 `k6` job 平行，只在 auth 相關路徑（`pkgs/platform/server`、`pkgs/platform/config`、`pkgs/identity`、`apps/rpc`、`configs`、`tests/k6`）變動時跑；壞掉的 HTTP response 那組由獨立的 `k6-wire` job 跑，跟所有產品 Go 變動；matrix 的 cell 名字可以接在 task 後面只跑幾個（`K6_AUTH_LIST=1 tests/k6/auth-matrix.sh` 列出全部 cell 名）。其他沒有列的組合（JWT、`optional` 模式、其他 procedure 的 public）沒有驗證，不保證。

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


### Upgrade notes — chart 0.3.73：移除 catalog 與 catalog-rpc（#518 切片 2）

這一版把 catalog bounded context 與 `apps/catalog-rpc` 從 repo 移除（程式、proto、Helm、console、k6、CI 與文件）。**不寫 DROP migration**，所以升級本身不會動到既有資料；要注意的是下面幾件事。

- **資料庫裡的 schema 原樣留下**：已部署環境的 `catalog` schema（`catalog.products`、`catalog.schema_migrations`）不會被刪；新安裝不會建立它（`apps/migrate` 不再跑 catalog 的 migration）。
- **要清除時是手動步驟**：先備份，確認沒有任何程式還在讀它，再執行：

```sql
DROP SCHEMA catalog CASCADE;
```

- **回滾界線**：catalog 在 chart 0.3.36（v0.54.0）起才是獨立部署的 catalog-rpc；0.3.35（v0.53.0）及更早，catalog 是 rpc 行程內的 service，chart 沒有 catalog-rpc 資源。不論哪一種，回滾到有 catalog 的版本都需要 `catalog` schema 與資料。若已 `DROP SCHEMA catalog`，**已核對的 v0.67.9（chart 0.3.72）**的 catalog-rpc 與 rpc 仍然起得來、readiness 也會是 200（啟動與 readiness 只 ping Postgres，不查 `catalog.products`），但第一次讀寫 catalog 才會因缺表失敗，rpc 對 catalog 的呼叫可能回 `catalog.unavailable`（catalog-rpc 5xx 會被映射成這個；實際取決於 catalog-rpc 缺表時怎麼失敗，本次沒有實測）。0.3.36 到 0.3.71 之間與 0.3.35 以前的版本沒有逐版核對 readiness 行為，不要假設一樣。所以健康檢查通過不代表回滾成功；要讓 catalog 請求恢復，必須還原 schema 與資料（從 DROP 前的備份）。清除 schema 之前，先確認不需要回滾。
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
- **Grafana**：Saga／Payment 面板列與 saga-pending、reservation-expiry、commit-short、payment-* alert 已移除；eventbus／auth／identity 的面板與 alert 保留。
- **依 consumer lag 擴縮 worker**：目前沒有載具（沒有 BC 發 event）；`helm-kind-keda` 只驗 ScaledObject 能 render、被 KEDA 接受且 `--wait` 成功，待切片 3 的示範 BC。

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
| identity bootstrap（不在 chart 裡） | 第一個 account + API key：在**操作者本機**跑一次 `worker bootstrap`，經 `kubectl port-forward` 連 release 的 DB，secret 只印在你自己的 terminal 一次（做法見下面「identity bootstrap」）；**不要在叢集裡跑**——Pod 的 stdout 會進 Pod log、也可能被 log collector 收走。config `auth.api_keys` 留作 break-glass。已有 account 就是 no-op，但不要同時跑兩個 |
| auth.oidc（`config.auth.oidc`） | 外部 OIDC 登入的 values，原樣進 ConfigMap：`enabled`（預設 `false`，關著時整段不讀）、`issuers[]`（`issuer` https、`audience`＝我們的 client id、`mapping` `bound`\|`auto_provision`、dev 才用的 `allow_insecure_issuer`）。**chart 不含 IdP**——Keycloak / Auth0 / Entra 自備；Dex 只在 kind（`deployments/helm/kind/dex.yaml`，見「Dex dev IdP」）。`auth.jwt` 也有設時，`auth.jwt.issuer` 要釘住且不得等於任一 OIDC issuer，否則 Pod 起不來。list 不能用 env 覆蓋，只能寫在 values。開著時 auth interceptor 用 verifier 驗 Bearer、再以 `(iss, sub)` 對應 account（未綁定只能 `ClaimInvite`，見 `docs/identity.md`「外部身分綁定」） |
| HPA / PDB / Ingress | rpc：CPU HPA（開了就不 render `replicas`、要有 `resources.requests.cpu`）、PDB 只在可能 >1 replica 時 render、Ingress 走 HTTP/1.1（Connect / gRPC-Web），原生 gRPC 要另外 h2c |
| console Deployment（`console.enabled`，預設 off） | `web/apps/console` 的 nginx image：chart 算出來的 env 只有 `RPC_UPSTREAM=http://<release>-rpc:<rpc.service.port>`，**沒有** `APP_CONFIG`、沒有 Secret——nginx 看不到 Postgres URL 和 API key；auth 開著且沒有 OIDC 時使用者在 console 的 Credential 對話框貼自己的 key / JWT。OIDC 登入的設定（#273）走 `console.extraEnv`：`CONSOLE_OIDC_ISSUER`、`CONSOLE_OIDC_CLIENT_ID`、`CONSOLE_API_KEY_LOGIN`（`false` 關掉 API key 入口）——entrypoint 渲染成 `/config.json`（只有這三個公開值），並把 issuer 的精確 origin 加進 CSP `connect-src`；沒設或格式不合就是 OIDC 關閉。IdP 的 client 要登記 redirect URI `https://<console host>/auth/callback`，並允許 console origin 的 CORS（見 [identity-login.md](identity-login.md) §8）；probe 打 nginx 自己的 `/healthz`（rpc 的健康是 rpc probe 的事）；同一組 `podSecurityContext`（uid 65532、read-only rootfs）+ `/tmp` emptyDir 就夠。自己的 Service / Ingress（`console.ingress`，純 HTTP/1.1） |
| ServiceMonitor（`rpc.serviceMonitor.enabled`，預設 off） | Prometheus Operator 抓 rpc Service 的 `http` port、path `/api/metrics`（gin route，auth 開著也不擋）；operator 靠 label 選 ServiceMonitor，`serviceMonitor.labels` 放它要的（kube-prometheus-stack 是 `release: <name>`）。只管 rpc——worker 沒 HTTP server，它的 metrics 只能走 `config.otel.endpoint` |
| NetworkPolicy（`networkPolicy.enabled`，預設 off） | 每個 Deployment 一張、只管 ingress（egress 不碰：DB / Redis / collector 在哪是 cluster 的事）。rpc 永遠放行同 release 的 console pod，加上 `networkPolicy.rpc.ingressFrom`（NetworkPolicyPeer list；**空 = 該 port 對任何來源開**，要收緊就列 ingress controller 與 Prometheus 的 namespace）；worker 不收任何 ingress；console 同一套 `console.ingressFrom`。要 CNI 真的實作 NetworkPolicy 才有效，kind 預設的 kindnet 不會，所以 helm-kind 驗不了，check.py 驗形狀 |
| worker autoscaling | replica 共用 `consumer_group` 就是分工（加 replica 是否有效取決於 broker，見「Worker 輪詢間隔」的「要不要調」），但 handler 是 DB-bound，CPU 低時 lag 照樣長，所以 `worker.autoscaling.metric` 預設 `lag`：KEDA `ScaledObject` 用 Prometheus trigger 查 `max(eventbus_consumer_lag{consumer_group=~"<group>\\..*"})`（gauge 是 per replica，取 max 不 sum；要 `config.otel.endpoint`，否則拒絕 render）；沒 KEDA 的 cluster 用 `metric: cpu` 出普通 HPA。實測踩到的三件事都做成 render 時就擋：`lag.prometheusAddress` 要 `http://<svc>.<ns>.svc:<port>`（KEDA operator 從**它自己的** namespace 解析，裸 Service 名不通）；`minReplicas` 必須 ≥ 1（lag gauge 是 worker 自己報的，0 個 replica 就沒人報）；`pollingInterval` / `cooldownPeriod` 只對 scale-to-zero 有效，縮回去的節奏是 HPA 的 `behavior.scaleDown`（預設 stabilization 300s，`worker.autoscaling.behavior` 透傳） |

**Migration 通則**：migration 在**一個交易**裡跑；`ALTER TABLE` 取 ACCESS EXCLUSIVE lock 並持有到整個 migration commit，該表的**讀與寫**都會等到它結束，大表請排維護窗口（窗口要涵蓋讀流量）。順序是先 migrate 再升 rpc／worker（chart 的 `pre-upgrade` hook 已保證）。golang-migrate 在執行檔案之前就把 `<schema>.schema_migrations` 標成 `<N> dirty=true`，所以失敗之後每次 `migrate up` 都拒絕（`Dirty database version N`）：排除原因後把版本設回 `N-1` clean 再重跑。**不要用 `task migrate -- down` 退單一 BC**——它對每個 BC 各退一步（每個 BC 的最新 migration 都會回滾）；只退一個 BC 時，在該 BC 的資料庫以專屬的 rollback runbook 執行（`BEGIN; …; UPDATE <schema>.schema_migrations SET version = <N-1>, dirty = false; COMMIT;`）。目前沒有 BC 有 rollback runbook 案例，新增時的要求見 `docs/workflow.md`「單一 BC 的 rollback runbook」。

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
  --set config.otel.endpoint=otel-collector:4317   # image 是 public 的 ghcr，不用 pull secret
```

`check.py` 是 chart 裡唯一有邏輯的地方的測試：rpc 的 grace period 算式與 env、readiness period 的關係、worker 沒 probe、migrate hook
與它的 Secret 的順序、prune 的 args、console 的 `RPC_UPSTREAM` 指到這個 release 的 rpc Service 且沒有 `APP_*` / Secret。ServiceMonitor 選到 rpc Service 的 port、NetworkPolicy 的 peers（console 一定在、`ingressFrom` 原樣、空就不出 `from`）。`ci/` 六個 profile（預設 + DB；redis broker + HPA + ingress + auth required；existingSecret；console + 自己的 ingress；hardened：NetworkPolicy 列 ingress-nginx / monitoring + ServiceMonitor + console；oidc：auth required + 兩個 issuer，驗 `auth.oidc` 原樣進 ConfigMap、布林沒被 YAML 1.1 吃掉）。
在 CI 的 `helm` job 跑。真的裝一次是 `task helm-kind`（CI 的 `helm-kind` job）：起 kind cluster、`deployments/helm/kind/deps.yaml`
放一個拋棄式 Postgres + Redis（chart 沒有 subchart dependency，production 接 managed DB，這份只給 smoke 用）、從 working tree build 三個 Go image（rpc / worker / migrate）+ console image（`web-build` → nginx）
`kind load` 進去、`helm upgrade --install --wait` 兩次（第一次走 pre-install hook、第二次走 pre-upgrade），每次跑 `kind/smoke.sh`：
rpc 的 `/api/readyz` 與 `/api/version`；console 回 `index.html`、`/api/readyz` 經 nginx proxy 到 rpc；Dex 的 discovery `issuer` 是約定的那個字串。本機約 1 分鐘（cluster 已存在時），`task helm-kind-down` 刪 cluster。
另一條 `task helm-kind-keda`（CI 同一個 job 的另一個 matrix leg，和 `helm-kind` 平行、各自一個 cluster；兩者共用的底座是 `task helm-kind-up`：cluster、image、Postgres / Redis）：裝 KEDA、`kind/monitoring.yaml` 放一個 OTLP collector（gRPC :4317 → Prometheus exporter :8889）
和一個 5s scrape 的 Prometheus，用 `kind/keda-values.yaml` 重裝 chart（`OTEL_METRIC_EXPORT_INTERVAL=5000`、threshold 50、scaleDown stabilization 30s），
並 `--wait`：驗 ScaledObject 能 render、被 KEDA 接受且 `--wait` 成功。「依 consumer lag 實際擴縮 worker」暫時沒有載具（沒有 BC 發 event，原本的擴縮實測已移除），等切片 3 的示範 BC。
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
