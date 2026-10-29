# Config

<sub>← [README](../README.md)</sub>

## Config（viper）

`platform/config.Load()` 的來源由低到高：defaults → YAML 檔 → `APP_*` 環境變數。YAML 找 `$APP_CONFIG`（指定就必須存在），
否則依序找 `./configs/config.yaml`、`./config.yaml`，都沒有也沒關係。每個 key 都有 default，所以只靠環境變數就能跑。

`configs/` 裡：

| 檔案 | 用途 |
|---|---|
| `config.example.yaml` | 有 commit 的範本，內容就是本機開發 profile（text log、`/api/docs` 開、`postgres.url` 指向 compose） |
| `local.yaml` | gitignored。`task run` / `task migrate` 第一次會從 example 複製過來（`task config`），之後透過 `APP_CONFIG` 載入；隨意改 |
| 其他 | `configs/*` 全部 gitignored；`task run CONFIG=<file>` 可指定別的檔 |

> 注意：env 不能把 key 清成空字串（viper 忽略空 env），所以要「不接 DB」請改 local.yaml 的 `postgres.url`，不要 `APP_POSTGRES_URL=`。

| YAML key | 環境變數 | 預設 | 說明 |
|---|---|---|---|
| `http.addr` | `APP_HTTP_ADDR` | `:8080` | |
| `http.docs` | `APP_HTTP_DOCS` | `false` | 掛 `/api/docs`、`/api/openapi/<bc>.yaml`（`task run` 會開） |
| `http.shutdown_delay` | `APP_HTTP_SHUTDOWN_DELAY` | `0s` | SIGTERM 後 `/api/readyz` 先回 503、其他照常服務這麼久，讓 LB 先把流量移走再關 listener；k8s 下設 ≥ 一個 readiness probe period |
| `http.timeout` | `APP_HTTP_TIMEOUT` | `10s` | 單一 RPC 的 server-side deadline（timeout interceptor）：usecase 的 context 到期、client 收 `DEADLINE_EXCEEDED` + `x-error-code: request.timeout`；`0` = 不限。client 自己帶的 `Connect-Timeout-Ms` 較短時以它為準 |
| `http.rate_limit.rps` / `.burst` / `.by` | `APP_HTTP_RATE_LIMIT_RPS` … | `0` / `0` / `global` | token bucket（rate limit interceptor，在 auth 之外）：`rps` 0 = 關；`burst` 0 = rps 無條件進位；`by: global` 全 server 一桶、`peer` 每個 client IP 一桶（連線的 peer address；proxy 沒保留來源時等於大家同一桶）。超過 → `RESOURCE_EXHAUSTED` + `x-error-code: request.rate_limited` + `Retry-After`（秒） |
| `http.shutdown_timeout` | `APP_HTTP_SHUTDOWN_TIMEOUT` | `15s` | listener 關掉後 in-flight request 的完成上限，超過就切斷、`Run` 回 error。`terminationGracePeriodSeconds` 要 > delay + timeout |
| `log.level` / `log.format` | `APP_LOG_LEVEL` / `APP_LOG_FORMAT` | `info` / `json` | |
| `auth.mode` | `APP_AUTH_MODE` | `off` | `off` 不掛 auth interceptor；`optional` 有帶 credential 才驗（沒帶 = anonymous，帶了驗不過一樣是錯）；`required` 沒有合法 credential → `UNAUTHENTICATED`。開著但 `api_keys` 與 `jwt` 都空是合法的（只靠 identity 的 DB key，`server.New` 會 Warn）；identity 表空的話就是沒人進得來 |
| `auth.api_keys` | `APP_AUTH_API_KEYS` | 空 | `name=key[:role\|role],name=key`（key ≥ 16 字元），對 `X-API-Key` header；principal 的 subject = name、roles = 冒號後的清單。一個字串，所以 env 帶得動。這是 static / break-glass 那組：對不上時 interceptor 再問 `pkgs/identity` 的 DB key（`identity.v1.IdentityService`，見 `docs/identity.md`），所以 mode 開著、這裡空著也合法——第一把 admin key 仍建議放這裡 bootstrap |
| `auth.admin_role` | `APP_AUTH_ADMIN_ROLE` | `admin` | `admin.v1.AdminService`（poison 操作）、`InventoryService.ExpireReservations`、`identity.v1.IdentityService`（除 `WhoAmI`）要求的 role；也是 ordering ownership 的 admin（可替任何 customer 下單、列全部訂單，見 `docs/identity.md`）；`auth.mode: off` 時不檢查 |
| `auth.jwt.hs256_secret` / `auth.jwt.public_key` | `APP_AUTH_JWT_HS256_SECRET` / `APP_AUTH_JWT_PUBLIC_KEY` | 空 | 二選一：HS256 secret（≥ 32 bytes）或 PEM public key（RSA / EC / Ed25519，演算法家族釘死在 key 型別上）。對 `Authorization: Bearer <jwt>`，`exp` 必填 |
| `auth.jwt.issuer` / `auth.jwt.audience` | `APP_AUTH_JWT_ISSUER` / `APP_AUTH_JWT_AUDIENCE` | 空 | 設了就檢查 `iss` / `aud` |
| `auth.public` | `APP_AUTH_PUBLIC` | 空 | 不需 credential 的 procedure 清單（`/ordering.v1.OrderService/GetOrder`，env 用逗號分隔） |
| `auth.resolver_timeout` | `APP_AUTH_RESOLVER_TIMEOUT` | `2s` | 一次 `server.CredentialResolver` 查詢（identity BC 的 DB key）的上限。auth 在 `http.timeout` 之外，沒有它 DB 卡住會拖住所有帶 key 的 request 與 shutdown drain；到期 = 拒絕（`auth.invalid_credential`）。`0` = 不限 |
| `postgres.url` | `APP_POSTGRES_URL` | 空 | 空 = in-memory；`apps/migrate` 必填 |
| `postgres.expect_target` | `APP_POSTGRES_EXPECT_TARGET` | 空 | `host:port`。有設時，`NewDB` 在連線前檢查 `postgres.url`：主連線與每個 fallback host 都必須是這個目標（含 `host=` 覆寫），不符就回錯誤、不連線；錯誤只帶這個 `host:port`，不帶 URL 的任何片段。空 = 不檢查（預設）。只檢查目標，不強制 TLS（TLS 規則只屬於 `worker bootstrap`）。與 `worker bootstrap --expect-target` 共用同一個檢查。也適用 `apps/migrate` 的 up／down／version（同一個 `postgres.Up/Down/Version`），同樣只 parse 一次 |
| `postgres.max_open_conns` | `APP_POSTGRES_MAX_OPEN_CONNS` | `25` | 這個 process 對 Postgres 最多開幾條連線（`sql.DB.SetMaxOpenConns`）；超過的查詢排隊等空的。`apps/rpc`、`apps/worker`、`worker` 子命令、`apps/migrate` 都經 `NewDB`，都吃這個。**`0` = 不設上限**（database/sql 的原預設，這裡是明確的 opt-out）；負值被拒。整個部署的總連線數 ≈ replica 數 × 這個值，必須小於 Postgres 的 `max_connections`（見 [operations.md](operations.md)）。`apps/worker` 另外在啟動時拒絕低於 `2 × subscriber 數 + 1` 的值（5 個 subscriber = 11）：每個 subscriber 從領到訊息到 ack 一直佔著一條交易連線，handler 的 use case 又要另一條，池子太小會全部卡在等連線、撞 `worker.handler_timeout` 後進 poison |
| `postgres.max_idle_conns` | `APP_POSTGRES_MAX_IDLE_CONNS` | `10` | 閒置時保留幾條連線（`SetMaxIdleConns`）；`0` = 用 database/sql 的預設（2）。`max_open_conns` 有設上限時，不得大於它 |
| `postgres.conn_max_lifetime` | `APP_POSTGRES_CONN_MAX_LIFETIME` | `30m` | 一條連線用多久後換掉（`SetConnMaxLifetime`），讓 failover、DNS / pgbouncer 變動後池子不會一直黏在舊 backend；`0` = 不換 |
| `postgres.conn_max_idle_time` | `APP_POSTGRES_CONN_MAX_IDLE_TIME` | `5m` | 閒置多久後關掉（`SetConnMaxIdleTime`），讓尖峰多開的連線還給 server；`0` = 不關 |
| `otel.endpoint` | `APP_OTEL_ENDPOINT` | 空 | OTLP/gRPC `host:port`；空 = telemetry 關 |
| `otel.insecure` | `APP_OTEL_INSECURE` | `false` | plaintext gRPC（本機 collector） |
| `otel.service_name` | `APP_OTEL_SERVICE_NAME` | 空 | `service.name`；空 = 執行檔名 |
| `worker.consumer_group` | `APP_WORKER_CONSUMER_GROUP` | `worker` | `apps/worker` 的 offset cursor 前綴，實際 group 是 `<值>.<bc>`（每個 BC 各自讀完整個 topic）；同名 replica 分工，新名字從哪開始看 `worker.start_from` |
| `worker.start_from` | `APP_WORKER_START_FROM` | `beginning` | **還沒有 cursor** 的 group 從哪開始：`beginning` 重播所有留存的訊息，`latest` 只收建立之後的。已有 cursor 的 group 不會被移動；redis 模式也套用到 forwarder 的 SQL group 與 stream group |
| — | `APP_CONFIG` | 空 | 指定 config 檔路徑（Taskfile 的 `CONFIG` var 會設它） |
| `redis.url` | `APP_REDIS_URL` | 空 | 只在 `worker.broker: redis` 時連（啟動 ping）；預設 broker 下 worker 完全不碰它 |
| `worker.broker` | `APP_WORKER_BROKER` | `postgres` | `postgres` 直接讀 outbox 表；`redis` 由 worker 轉送進 Redis Streams 再讀 |
| `worker.handler_timeout` | `APP_WORKER_HANDLER_TIMEOUT` | `10s` | 單次 handler attempt 的 deadline（`http.timeout` 的 consumer 版）：usecase 的 context 到期、該次算 failed 照常 retry，用完後以 `handler timed out` 為 reason 進 poison；`0` = 不限。SIGTERM 時 in-flight handler 的上限是它和 drain 的 8s 取短 |
| `worker.poll_interval` | `APP_WORKER_POLL_INTERVAL` | `200ms` | 每個 Postgres subscriber 在查不到訊息後，隔多久再查一次 outbox 表（watermill-sql `PollInterval`）；必須 > 0。空輪詢之後才會等滿一個間隔，所以新事件在等待期間到達的額外延遲是 0 到約一個間隔（忙碌時下一次查詢不等）；它也是 DB 輪詢 QPS 的來源（`輪詢迴圈數 × replica 數 ÷ 間隔`；postgres broker 的迴圈數是 handler subscription 數，redis 模式是 forwarder 的 distinct topic 數，見 [operations.md](operations.md)）。只影響 `postgres` broker 與 redis 模式 forwarder 的 SQL 端；重送等待與查詢失敗的退避仍是 library 的 1s |
| — | `TEST_DATABASE_URL` | 未設 | 只給 integration test：設了就用該 DB，未設則 testcontainers 自起 |
| — | `TEST_REDIS_URL` | 未設 | 同上，給 worker E2E 的 redis 模式 |
