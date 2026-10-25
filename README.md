# x-monorepo

Protobuf-first Go monorepo：**Clean Architecture + DDD（BC-first）**，go.work 多 module，
Connect 對外，Wire 組裝，Turborepo 前端。

## Use this template

這是 GitHub template repo。按 **Use this template** 建好自己的 repo 後：

```bash
task tools                                  # 固定版本的工具鏈 → ./bin
task rename -- github.com/<you>/<repo>      # 改 module path（go.mod / imports / buf.gen.yaml / depguard）並重新 gen
git commit -am "chore: rename module path"  # gen-freshness 是跟 HEAD 比，先 commit
task gate-ci                                # 跟 CI 一樣的檢查，應該全綠
```

`ordering` 是示範用的 BC，熟悉結構後可以照「移除一個 BC」那節拆掉。

設計的來源與取捨：

| 來源 | 借來的 | 刻意不借的 |
|---|---|---|
| DDD / go-ddd | aggregate、VO、repository in domain、BC-first、ACL、event | — |
| go-kit | Service / Endpoint / Transport 三層；endpoint middleware → `usecasex` decorator | 手寫 transport、`any` 型別擦除、god service interface |
| go-micro | 一個 service 可同時掛多種 transport；broker / store 抽象為 port | 框架本身、自家 codegen、內建 registry（K8s 已有） |

## Quick start

```bash
task tools          # buf / protoc-gen-go / protoc-gen-connect-go / wire / golangci-lint → ./bin
task tidy           # 每個 module go mod tidy + go work sync（產生 go.sum）
task gen wire       # protos → pkgs/*/contract、DI graph → apps/*/wire_gen.go
task lint test      # depguard 強制分層與 BC 邊界
task compose-up     # 本機 Postgres + otel-lgtm（Grafana http://localhost:3000）
task migrate -- up  # 跑每個 BC 的 migrations（service 啟動時不會自己跑）；第一次會把 config.example.yaml 複製成 configs/local.yaml（gitignored）
task run            # ./bin/rpc 讀 configs/local.yaml：Connect + gRPC + gRPC-Web 同一個 port、text log、/api/docs 開
                    # 不想接 DB：把 local.yaml 的 postgres.url 清空（= in-memory repository）

curl -X POST localhost:8080/api/ordering.v1.OrderService/CreateOrder \
  -H 'Content-Type: application/json' \
  -d '{"customerId":"c-1","items":[{"sku":"SKU-1","quantity":2,"unitPrice":{"amountMinor":"18000","currency":"USD"}}]}'
```

## Layout

```
.
├── go.work                        # 每個 pkgs/* 和 apps/* 各一個 go.mod
├── buf.yaml / buf.gen.yaml        # proto module；go_package 由 override 決定輸出位置
├── buf.gen.ts.yaml                # TS 輸出到 web/packages/proto
├── protos/
│   ├── common/v1/money.proto      # 跨 BC 的 wire 型別
│   ├── ordering/v1/
│   │   ├── entity.proto           # aggregate 的 state schema（跨語言契約，不是 Go domain model）
│   │   ├── service.proto          # RPC
│   │   └── event.proto            # 進出 broker 的訊息
│   └── inventory/v1/              # 第二個 BC 的契約（entity / service / event）
├── pkgs/
│   ├── kernel/                    # shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
│   ├── platform/                  # 技術共用：contextx、config、server（gin）、usecasex、metrics、postgres（gorm + migrate runner）
│   └── ordering/                  # ── Bounded Context（完整六角形）──
│       ├── provider.go            # wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorator 包起來
│       ├── contract/v1/           # buf 生成：pb + Connect + openapi.yaml（只有 adapters / apps 可 import）
│       ├── domain/order/          # 手寫 aggregate、VO、Repository interface、domain errors
│       ├── application/
│       │   ├── dto/               # application 邊界（go-kit endpoint request/response）
│       │   ├── usecase/           # orchestration（go-kit service）
│       │   ├── port/              # 需要外界提供的能力
│       │   └── mapper/            # domain → dto
│       ├── infra/                 # driven：repository / port 實作
│       │   ├── memory/            # in-memory repository（APP_POSTGRES_URL 未設時）
│       │   ├── postgres/          # gorm row model + SQL + 錯誤轉譯；migrations/*.sql 以 embed 隨 BC 走
│       │   └── system/            # clock / id
│       └── adapters/              # driving：transport
│           ├── rpc/               # Connect handler；pb ↔ dto
│           ├── consumer/          # event handler；呼叫同一個 CancelOrder usecase
│           └── events/            # outbound：domain event → proto → Watermill publisher
│   └── inventory/                 # ── 第二個 BC：同樣結構 ──
│       ├── domain/stock/          # Stock aggregate（每 SKU 一筆，optimistic version）
│       ├── domain/reservation/    # Reservation aggregate（每 order 一筆：RESERVED / REJECTED / RELEASED）
│       └── adapters/consumer/     # 消費 ordering.v1.OrderCreated / OrderCancelled（只 import ordering 的 contract/）
├── deployments/docker/            # docker-compose.yaml：本機 infra（Postgres），app 不進容器；goreleaser 用的 Dockerfile
├── deployments/helm/x-monorepo/   # Helm chart：rpc / worker Deployment、migrate hook Job、prune CronJob（task helm-check）
├── deployments/helm/kind/         # task helm-kind：kind cluster 真裝 chart（拋棄式 Postgres / Redis + smoke.sh）
├── tests/k6/                      # k6：orders（load + thresholds）、ratelimit、timeout、reservation（worker 鏈延遲）；task k6 -- <script>、task k6-smoke
├── apps/
│   ├── rpc/                       # composition root：main + wire，無業務、無 mapping
│   ├── worker/                    # 事件 consumer process：同一份 ProviderSet + Watermill router
│   └── migrate/                   # 收集各 BC 的 embed migrations，cobra：up / down / version；唯一能 import infra 的 app
└── web/                           # Turborepo（pnpm）
    ├── packages/proto/            # @repo/proto：buf 生成的 connect-es client（有 commit）
    └── apps/console/              # Vite + React + TanStack Router / Query + Tailwind v4 + shadcn/ui
```

## Dependency rules（`.golangci.yml` depguard 強制）

| 套件 | 可以 import | 不可以 import |
|---|---|---|
| `domain` | `kernel` | application / infra / adapters / contract / platform |
| `application` | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| `infra` | domain、application/port、platform | adapters / contract |
| `adapters` | application、contract、platform、**其他 BC 的 `contract/`**（consumer 解它的 event、events adapter 發它擁有的 request） | 其他 BC 的 domain / application / infra / adapters |
| `kernel`、`platform` | 標準庫、第三方 | 任何 BC |
| `apps/*` | 各 BC 的 `provider.go`、platform | 直接呼叫 usecase（只透過 wire）、infra |
| `apps/migrate` | 各 BC 的 `infra/postgres`（只為了拿 `Migration()`）、platform | domain / application / adapters |
| 任何 BC 的 domain / application / infra | — | 其他 BC 的任何東西（含 contract；走 port 或 event） |

**contract 和 dto 的差別**：contract 是 wire format（有版本、跨語言、生成），dto 是 application 邊界（手寫、
只放 usecase 需要的欄位）。同一個 usecase 被 rpc 和 consumer 兩個 adapter 呼叫時，dto 就是它們的共同語言。
dto 不要照著 pb 抄。

## server：gin + Connect

`platform/server` 用 gin 當 HTTP router：每個 BC 的 Connect handler 以 `gin.WrapH` 掛在 `/api/<pkg>.<Service>/*procedure`
（`http.StripPrefix`，給 Connect / gRPC-Web client 用 base URL `…/api`）**以及**原生路徑 `/<pkg>.<Service>/*procedure`
（gRPC 協定把路徑寫死，grpc-go / grpcurl 加不了前綴）；`/api/healthz`（liveness：process 活著就 200，不看依賴）、`/api/readyz`（readiness：跑 app 給的 `server.Readiness` checks，
rpc 是 Postgres ping；任一失敗或 server 正在 drain 回 503 + JSON 明細）、`/api/version`（`{version, commit, date, go}`，不用驗證：port-forward 就知道 pod 跑哪版；console 的 footer 顯示「console X · server Y」——X 是 release.yml build 時用 `VITE_CONSOLE_VERSION` 烘進去的 tag，本機 build 是 `dev`；兩邊都是真版號又不同就標 version mismatch）、`/api/metrics`、`/api/docs` 是普通 gin route。
**Graceful shutdown**（`Server.Serve`，`apps/rpc/main.go` 以 `signal.NotifyContext` 接 SIGINT / SIGTERM）：先把 `/api/readyz` 翻成 503 `"server":"draining"`
但其他路徑照常服務 `http.shutdown_delay`（預設 0：本機 Ctrl-C 立刻關；k8s 設 ≥ 一個 probe period，LB 才來得及停止派流量——Helm chart 的 `rpc.shutdown` 把這個關係算死），
再關 listener、給 in-flight request `http.shutdown_timeout`（預設 15s）跑完；超時才切斷並回 error。liveness 全程 200。
HTTP/1.1 與 h2c 各一個 in-flight request 的 drain 有真 socket 的測試（`platform/server/shutdown_test.go`），SIGTERM → exit 0 有 `apps/rpc/shutdown_test.go`。
整台 server 都在 `/api` 下，ingress 一條規則。`RedirectTrailingSlash` / `RedirectFixedPath` 關閉（gRPC client
不跟 redirect）；gin 只做 routing + `Recovery()`，RPC log 留在 Connect interceptor。HTTP/1.1 與 h2c 同一個 port，
Connect / gRPC / gRPC-Web 三種協定都有 E2E test（`apps/rpc/main_test.go`）。

**Response meta 走 header，不包進 proto message**：transport 層本來就有 status / header / trailer，在 body 包 `{data, meta}`
等於重做一次、每個 Response 都要重複寫、client 多剝一層；錯誤的 `x-error-code` 也已經是 header。`server.NewMetaInterceptor`
在成功與錯誤兩條路都設：

| header | 內容 |
|---|---|
| `X-Request-Id` | client 有帶就原樣回（≤128 字），沒帶就 server 產 UUID；`server.RequestID(c)` 拿得到，request log 也有 `request_id` |
| `X-Trace-Id` | OTel trace id（有 span 才有），直接貼到 Tempo / Loki `| trace_id="…"` |
| `traceparent` | W3C trace context，`otelconnect.WithPropagateResponseHeader()` 回給瀏覽器端 tracing（otelconnect 只在成功回應設；錯誤看 `X-Trace-Id`） |

分頁之類「該 RPC 自己的資料」（`next_page_token`、`total_size`）才放進 message，不做通用 `ResponseMeta` 欄位。

## Error codes：`x-error-code` 總表

每個失敗的 RPC 都帶 Connect code（HTTP status 由它決定）+ header `x-error-code`，client 判斷用後者（`console` 的 `describe()` 就是印它）。
來源三種：**platform interceptor**（`request.*` / `auth.*`）、**admin**（`admin.*`，`pkgs/platform/admin` 自己設）、**domain error**
（`kernel.NewError(kind, code, msg)`，`server.ToConnectError` 依 `Kind` 映射：InvalidArgument → `INVALID_ARGUMENT`、NotFound → `NOT_FOUND`、
FailedPrecondition → `FAILED_PRECONDITION`、Conflict → `ALREADY_EXISTS`；其他 → `INTERNAL`，**沒有** `x-error-code`，訊息固定 `internal error`）。
加新的 domain error 就在該 aggregate 的 `errors.go` 加一行，前綴 = aggregate 名；這張表是手寫的，改了記得同步。

| `x-error-code` | Connect code | 誰設 | 什麼時候 |
|---|---|---|---|
| `request.validation_failed` | `INVALID_ARGUMENT` | validate interceptor | request 違反 proto 的 `buf.validate` 規則；`Violations` detail 列欄位（rule id 如 `admin.poison_offsets_xor_all` 也在裡面） |
| `request.timeout` | `DEADLINE_EXCEEDED` | timeout interceptor | 超過 `http.timeout`，不管底下是什麼錯 |
| `request.rate_limited` | `RESOURCE_EXHAUSTED` | rate limit interceptor | 超過 `http.rate_limit`，另帶 `Retry-After` |
| `auth.unauthenticated` | `UNAUTHENTICATED` | auth interceptor | `auth.mode=required` 且沒帶 credential |
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了但對不上（未知 API key、JWT 簽章 / exp / iss / aud 不對） |
| `auth.permission_denied` | `PERMISSION_DENIED` | `server.RequireRole` | 有身分但缺角色（AdminService 要 `auth.admin_role`） |
| `concurrent_update` | `ABORTED` | `server.ToConnectError` | optimistic lock 撞版（`order.ErrStale` / `stock.ErrStale`，wrap `kernel.ErrConcurrentUpdate`）；重送即可，刻意沒有前綴也不是 `kernel.Error` |
| `pagination.invalid_page_size` | `INVALID_ARGUMENT` | `platform/pagination` | `page_size` 負數 |
| `pagination.invalid_token` | `INVALID_ARGUMENT` | `platform/pagination` | `page_token` 解不開，或帶的 filter 跟這次 request 不同（ListOrders / ListStocks 把 filter 編進 token） |
| `admin.no_database` | `FAILED_PRECONDITION` | admin | server 沒接 Postgres（`postgres.url` 空）：poison 表與 outbox / inbox 都在 DB |
| `admin.unknown_bounded_context` | `INVALID_ARGUMENT` | admin | `bounded_context` 不在 `apps/rpc` 列的 schema 清單 |
| `admin.poison_not_found` | `NOT_FOUND` | admin | `GetPoison` 的 offset 不存在 |
| `order.invalid_id` / `order.empty_customer` / `order.no_items` / `order.invalid_sku` / `order.invalid_quantity` | `INVALID_ARGUMENT` | ordering domain | aggregate 不變量（consumer path 沒有 protovalidate，所以留著） |
| `order.invalid_status_filter` / `order.invalid_cursor` | `INVALID_ARGUMENT` | ordering domain | ListOrders 的 filter / cursor 壞掉 |
| `order.cannot_cancel` | `FAILED_PRECONDITION` | ordering domain | 不是 PENDING 的 order 被 Cancel |
| `order.not_found` | `NOT_FOUND` | ordering domain | |
| `order.duplicate` | `ALREADY_EXISTS` | ordering domain | 同 id 再 Save（repo 層） |
| `stock.invalid_sku` / `stock.negative_quantity` / `stock.invalid_quantity` | `INVALID_ARGUMENT` | inventory domain | SetStock / Reserve 的參數 |
| `stock.below_reserved` | `FAILED_PRECONDITION` | inventory domain | SetStock 想把 on-hand 壓到低於已 reserved |
| `stock.release_exceeds_reserved` | `FAILED_PRECONDITION` | inventory domain | Release 超過 reserved |
| `stock.not_found` | `NOT_FOUND` | inventory domain | |
| `reservation.invalid_order_id` / `reservation.no_lines` / `reservation.invalid_sku` / `reservation.invalid_quantity` | `INVALID_ARGUMENT` | inventory domain | aggregate 不變量 |
| `reservation.invalid_status_filter` / `reservation.invalid_cursor` | `INVALID_ARGUMENT` | inventory domain | ListReservations 的 filter / cursor |
| `reservation.not_reserved` | `FAILED_PRECONDITION` | inventory domain | 只有 RESERVED 能 Release |
| `reservation.stock_missing` | `FAILED_PRECONDITION` | inventory domain | Release 時某 SKU 的 stock 列不在了 |
| `reservation.not_found` | `NOT_FOUND` | inventory domain | `GetReservation`：worker 還沒處理這張 order（console 顯示 `none yet`） |
| `reservation.duplicate` | `ALREADY_EXISTS` | inventory domain | 同一 order 第二張 reservation（repo `Add`）；reserve / release usecase 把它當「已處理」吞掉，rpc 實際上看不到 |
| `message.duplicate` | （不到 rpc） | `usecasex.Idempotent` | consumer inbox 看到重送的 message id：handler 回它、broker ack、不 retry；rpc path 沒 message id 不會發 |
| `order.invalid_status` / `reservation.invalid_status` / `stock.corrupt_quantities` | `INTERNAL`（header 不帶 code） | domain | 從 DB 讀回來的狀態 / 數量不合法：資料壞了，不是 client 的錯 |

Kind → Connect code 只在 `server.ToConnectError` 一處；consumer 不看 code：`kernel.Error` 一律 drop（進 poison），其他 error retry。

## Validation：protovalidate 在邊界擋 request 形狀

| 層 | 擋什麼 | 位置 |
|---|---|---|
| proto | `buf.validate` rule 直接寫在 request message 上（`min_len`、`gt`、`gte`、`pattern`、`required`、`min_items`） | `protos/<bc>/v1/service.proto`、`common/v1/money.proto`、`ordering/v1/entity.proto` |
| server | `NewAuthInterceptor`（`auth.mode` 不是 `off` 才掛，在 logging 之內、validate 之外）：`X-API-Key` 對 `auth.api_keys`，否則 `Authorization: Bearer` 對 `auth.jwt`，解析成 `contextx.Principal{Subject, Name, Roles, Method}` 放進 context；`rpc` access line 帶 `auth`（`api_key` / `jwt` / `anonymous` / `public`）與 `sub`（auth 在 logging 之內，透過 context 裡的 slot 回報）、usecase 的 log 帶 `sub`、span 帶 `enduser.id` / `enduser.roles` / `auth.method`——Loki 與 Tempo 都查得到誰呼叫的，被拒絕的呼叫也有一行（`code=unauthenticated auth=anonymous`）；每個決定計入 `auth_decisions_total{result, method}`（dashboard 的 Auth row 與 `rpc-auth` alert 看它）；name 與 credential 本身不記；usecase 用 `contextx.PrincipalFrom(c)` 讀（consumer path / mode off 是 anonymous zero value）。只做 authentication，授權是讀的人的事。沒帶 → `UNAUTHENTICATED` + `x-error-code: auth.unauthenticated`，帶了驗不過 → `auth.invalid_credential`。probes / `/api/metrics` / `/api/docs` 是 gin route，不在 RPC chain 裡，永遠開著 | `pkgs/platform/server/auth.go`、`pkgs/platform/contextx/principal.go` |
| server | `NewRateLimitInterceptor`（`http.rate_limit.rps` > 0 才掛，在 logging 之內、auth 之外——洪水要在付 JWT 簽章驗證成本之前就擋掉）：token bucket，`by: global` 一桶 / `peer` 每個 client IP 一桶（idle 1 分鐘回收）。沒 token → `RESOURCE_EXHAUSTED`、`x-error-code: request.rate_limited`、`Retry-After: <秒>`，不排隊等 | `pkgs/platform/server/limits.go` |
| server | `NewTimeoutInterceptor`（`http.timeout` > 0 才掛，在 auth 之內、validate 之外）：usecase 拿到會到期的 context，慢 query 被 cancel 而不是吃掉整個 shutdown drain 窗口。到期後失敗的 RPC 一律回 `DEADLINE_EXCEEDED` + `x-error-code: request.timeout`（不管 driver 的 error 被 `ToConnectError` 映成什麼）；到期後仍成功的照常回 response（工作已 commit）。client 的 deadline 較短時一樣適用 | `pkgs/platform/server/limits.go` |
| server | `NewValidateInterceptor`（最內層 interceptor）：違反 → `INVALID_ARGUMENT`、header `x-error-code: request.validation_failed`、error detail `buf.validate.Violations`（欄位路徑 + rule id + message）；rule 本身編不過 → `INTERNAL` | `pkgs/platform/server/validate.go` |
| domain | 業務不變量照舊（`NewMoney`、`NewLineItem`…）：consumer path 沒有 interceptor，domain 是唯一防線 | `pkgs/<bc>/domain` |

- 規則故意和 domain 重複：edge 給 client 好讀的錯（哪個欄位、哪條 rule），domain 給所有入口一致的保證。
- `buf.yaml` 的 `deps` 帶進 `buf.build/bufbuild/protovalidate`（`buf.lock` 有 commit）；`buf.gen.yaml` 用 `managed.disable` 讓它保留自己的 Go package。
- OpenAPI 會把 rule 轉成 `minLength` / `minimum` / `pattern`，所以 `/api/docs` 看得到。

## Pagination：cursor（keyset）+ opaque page token

列表 RPC 走 [AIP-158](https://google.aip.dev/158) 的欄位名（`page_size` / `page_token` / `next_page_token`），不做 offset：
`OFFSET n` 深頁是線性掃描，且中途插入或刪除會漏／重複；keyset 每頁都是 index range scan。`ListOrders` 是範例：

| 層 | 負責 | 位置 |
|---|---|---|
| domain | 排序鍵固定 `(created_at desc, id desc)`（UUID 不可排序，`id` 只當 tie-breaker）；`order.Cursor` 是型別，`ListParams` 帶 `Limit` / `After` | `domain/order/cursor.go`、`repository.go` |
| platform | `pagination.Clamp`（0 → 預設 20、上限 100、負數 → `InvalidArgument`）與 `EncodeToken` / `DecodeToken`（base64url(JSON)，不透明但不是秘密） | `platform/pagination` |
| usecase | token 在這裡鑄造與驗證（所有 adapter 共用同一契約）；payload **包含當時的 filter**，換了 filter 還帶舊 token → `pagination.invalid_token` | `usecase/list_orders.go` |
| infra | SQL `WHERE (created_at, id) < (?, ?) ORDER BY created_at DESC, id DESC LIMIT n+1`：多抓一筆判斷有沒有下一頁，不 COUNT；memory 版做一樣的比較 | `infra/postgres/order_repository.go`、migration `0002` 的兩個 composite index |

- `total_size` 不回：大表 COUNT 是另一筆成本，要時再加 optional 欄位不算 breaking。
- 篩選的「空值」語意：`customer_id` 空字串、`status = UNSPECIFIED` 代表不篩；但 server 不認識的 enum 值不會塌成「不篩」，
  而是 `order.invalid_status_filter`（列表 API 寬鬆化等於擴大資料暴露面）。
- 不開放 `order_by`：動態排序會讓 cursor 與 index 都要跟著變，有需求再加。

## Events：domain event → transactional outbox（Watermill）→ worker

x-brain pitfall `best-effort-publish-loses-one-shot-events`：「存完再 best-effort publish」掉一次就是永久掉。這裡的做法：

| 層 | 做什麼 | 位置 |
|---|---|---|
| kernel | `kernel.Event`（`AggregateID()`、`OccurredAt()`）是所有 BC 事件的 base interface | `pkgs/kernel/event.go` |
| domain | aggregate 在行為裡 `record()`（`New` → `Created`、`Cancel` → `Cancelled`），`PullEvents()` 取出並清空；`order.Event` embed `kernel.Event`，實作共用 embedded `base` struct | `domain/order/events.go` |
| application | usecase 只依序呼叫 `repo.Save` 與 `port.EventPublisher.Publish`；`provider.go` 的 `decorateTx()` 用 `usecasex.Transactional` 把整個 usecase 包進 `port.Transactor.Do`，兩者同一個交易 | `provider.go`、`usecase/create_order.go` |
| adapters/events | domain event → `ordering.v1.OrderCreated` / `OrderCancelled` proto bytes → `message.Publisher`；topic = proto full name，metadata 帶 `key`（aggregate id）與 `traceparent` | `adapters/events/publisher.go` |
| platform/eventbus | Watermill：`PostgresPublisher` 從 message context 取出 use case 的 `*sql.Tx` 寫 `<bc>.events_<topic>`（**outbox 就是 commit 本身**）；`NewPostgresSubscriber` + `NewRouter`（Recoverer → tracing → 把帶 topic / message_id / trace_id 的 logger 放進 context，usecase log 才會進 Loki）+ `Register`（每個 BC：PoisonQueue → Retry → drop → access → timeout：每則訊息一行 Info `message`，對應 rpc 的 `rpc` access log；`worker.handler_timeout` 給每次 attempt 一個 deadline）；`ProvidePublisher` 由 app 提供一次給所有 BC；沒 DB 時用 GoChannel（at most once、同 process） | `pkgs/platform/eventbus` |
| infra | `postgres.Transactor` 把 gorm tx 放進 context，repository / publisher 用 `platformpg.TxOrDB` 拿；memory 版直接呼叫 fn | `infra/postgres/transactor.go` |
| apps/worker | 另一個 process：wire 每個 BC 的 `ProviderSet`，把各 BC 的 `Subscriptions`（`ProvideSubscriptions`，named type 讓 wire 分得開）掛上同一個 router；每個 BC 自己的 subscriber / consumer group（`<worker.consumer_group>.<bc>`）與 poison topic。`task worker`，需要 `postgres.url`；同 `worker.consumer_group` 的 replica 分工。`worker poison …`（`task poison -- …`）是 poison 表的操作面、`worker prune`（`task prune -- …`）是 outbox / inbox 的 retention | `apps/worker` |

- Topic → table：`ordering.v1.OrderCreated` → `ordering.events_v1_ordercreated`（+ `_offsets`），第一段就是 BC，**DDL 由該 BC 的 migration 出**（`0003_events`），
  service 不在啟動時建表；`InitializeSchema` 關閉。新 topic = 新 migration。
- 語意：at-least-once。handler 回 `eventbus.ErrDrop`（壞 payload、domain 說不行 → 例如重送的取消要求碰到 `ErrCannotCancel`）= ack 不重試也不進 poison；
  其他 error → Retry（3 次、指數退避）→ `ordering.poison` 表（`events_poison`）。
- 冪等有兩層：**inbox**（`<bc>.inbox` 表，`usecasex.Idempotent` 在 `decorateTx` 的 transaction 之內把 message id 記進去，重送（broker at-least-once、
  `poison requeue`、ack 遺失）在載入任何 aggregate 之前就以 `message.duplicate` drop；失敗的 attempt 連記錄一起 rollback，retry 會重記）
  加上 domain 狀態機（同一筆 order 的第二個 `OrderCreated` → `reservation.duplicate`）。
  沒 DB（in-memory）時沒有 inbox：GoChannel 本來就 at most once。
- **Retention**：outbox 表與 inbox 表都只會長，`task prune -- --older-than 720h [--dry-run] [--unconsumed] [<bc>]`（`worker prune`）刪掉
  (1) 比 `--older-than` 老、且**每個**有 cursor 的 consumer group 都 ack 過的 outbox 列（判斷式就是 lag gauge / subscriber「還沒讀」的反面；
  redis 模式下 cursor 是 forwarder 的）——沒有任何 group 有 cursor 的表（只發不收的 topic、worker 從沒跑過）預設跳過，`--unconsumed` 才清；
  (2) 比 `--older-than` 老的 inbox 列，但任何 BC 的 poison 表還停著的 message id 留著（requeue 用原 id 重發，inbox 列是擋重做的憑據）。
  一列只要還有**一個**有 cursor 的 group 沒讀就留著，NOTE 欄會列出是誰擋的；不會再跑的 group（對這個 DB 跑過的 E2E 留下的 `test-*`、
  改過名的 `worker.consumer_group`、broker 改 redis 前的 BC group）用 `--forget-group <name>` 先刪掉它的 cursor（真回來就當新 group，照 `worker.start_from`）。
  poison 表不在此列（`poison drop` 才是）；redis stream 的修剪是 Redis 端的 XTRIM 政策。先 `--dry-run` 看數字再跑。
  同一件事也有 RPC：`AdminService.Prune`（`older_than` / `dry_run` / `unconsumed` / `forget_groups` / `batch_size`，回每張表的列數、skipped 原因、
  擋住的 group），console 的 **Prune** 頁就是打它——alert 響了之後不用進 pod：dry run 看 NOTE、填 forget、再跑；
bc / days / forget 在 URL 上（`/prune?bc=ordering&forget=test-old.ordering`，Poison 頁的 BC badge 與 alert 說明都連過來），dry run 永遠從開著開始。它跑在 `http.timeout` 底下，
  所以是 dry-run / forget-group / 例行小量的入口；第一次清大 backlog 用 CLI / CronJob（逾時只是回應掉了，已刪的 batch 都已 commit，重跑無害）。
- Poison / prune 操作面有兩個入口，底層同一組 `eventbus.ListPoison / Requeue / Drop / PruneSchema`：
  **RPC** `admin.v1.AdminService`（`protos/admin/v1`，platform 的 contract，不屬於任何 BC；`pkgs/platform/admin` 是 handler，
  `apps/rpc` 掛上並給它 BC schema 清單）— `ListPoison / GetPoison / RequeuePoison / DropPoison / Prune`，auth 開著時要 `auth.admin_role`
  （API key 用 `name=key:admin`，JWT 用 `roles` claim），不然 `PERMISSION_DENIED` + `x-error-code: auth.permission_denied`；
  `auth.mode: off` 時全開（跟其他 RPC 一樣——`off` 只給本機開發，任何外面連得到的環境都要 `required` + `admin_role`）。console 的 **Poison** 頁就是打它。
  **CLI** `task poison -- list [-n 50] [<bc>]` 列出各 BC 停在 poison 表的訊息（offset、uuid、原 topic、最後一次 error），
  `show <bc> <offset>` 看完整 metadata 與 payload（proto bytes，印 base64；用 `buf convert` 解），`requeue <bc> <offset>…|all`
  把訊息**重新 insert 回原 topic 的 outbox 表**（同一個 message id、拿掉 poison metadata；每個 consumer group、含 redis 模式的 forwarder
  都會再收到）並同一個 transaction 刪掉 poison row，`drop <bc> <offset>…|all` 直接刪。同一個 binary（cobra：沒有子命令 = 跑 consumer）：
  `worker poison …`，image 也是 `docker run …/worker poison list`，`--help` 有完整說明。`eventbus_poison_messages` 不為 0 就來這裡；修好 handler 再 requeue，再失敗會再進 poison。
- Trace：publish 時把 `traceparent` 寫進 metadata，router 的 tracing middleware 還原成 CONSUMER span，Tempo 看得到 rpc → publish → handler 一條線。
- **Graceful shutdown**：SIGTERM 後 router 停止取新訊息，但**正在跑的 handler 跑完、ack 寫進去**才結束（`eventbus.Draining` 包住每個 BC 的 subscriber
  與 redis 模式的 forwarder；上限 `eventbus.DefaultDrainTimeout` 8s，超過就 cancel 掉 handler 的 context 並放棄、訊息留給下一次）。
  不包的話 watermill-sql / redisstream 在 subscribe context 一 cancel 就把 message context 也 cancel、丟掉它的 ack：use case 的 transaction 中途失敗、重啟後再收一次。
  兩種 broker 都有測（`platform/eventbus/shutdown_test.go`），SIGTERM → exit 0 有 `apps/worker/shutdown_test.go`。k8s 的 `terminationGracePeriodSeconds` 要 > 18s（8s drain + router `CloseTimeout` 10s；Helm chart 的 `worker.terminationGracePeriodSeconds` 預設 30、低於 19 拒絕 render）。
- 換 broker：`message.Publisher` / `Subscriber` 是 port；handler 與 error contract 不變。已內建一個示範：`worker.broker: redis`
  （`redis.url` 必填，`task compose-up` 有起一個）時，worker 自己當 forwarder，把每個被消費 topic 的 outbox 表（Postgres consumer group
  固定叫 `forwarder`，所有 replica 共用，每則只轉一次）passthrough 進同名 Redis Stream，handler 再以 `<consumer_group>.<bc>` 的
  Redis consumer group 讀。**outbox 仍是寫入路徑**（use case 不直接發 Redis，否則 commit 失敗會留下幽靈事件），poison 也仍寫 Postgres 表。
  代價：forwarder ack 之後訊息只存在 Redis（要開 persistence，compose 已開 AOF）；SQL → Redis 是 at-least-once，重送的訊息帶同一個 UUID。
  既有環境第一次切到 redis 時，`forwarder` 這個 SQL group 與 `<group>.<bc>` 的 stream group 都還沒有 cursor，預設會把歷史 outbox 全部重播；
  不想要就設 `worker.start_from: latest`（stream group 在 `NewWorker` 裡先建好再啟動 forwarder，所以「latest」是從啟動那一刻算）。
  換 NATS / Kafka 同一個形狀：`eventbus` 多一組 publisher / subscriber，`NewWorker` 的 switch 多一個 case。
- 代價：watermill-sql 會把 `lib/pq` 一起連進來（只用它的 error type），實際連線仍走 pgx。

### 跨 BC 的示範：ordering ↔ inventory

```
CreateOrder ──► ordering.v1.OrderCreated ──► inventory ReserveStock（全有或全無）
                                              ├─ RESERVED → inventory.v1.StockReserved
                                              └─ REJECTED → ordering.v1.OrderCancellationRequested ──► ordering CancelOrder
CancelOrder ──► ordering.v1.OrderCancelled ──► inventory ReleaseStock（RESERVED → RELEASED；找不到 → CANCELLED tombstone；否則 domain error → drop）
```

- inventory 只 import `ordering/contract/v1`（consumer 解 `OrderCreated`、events adapter 發 `OrderCancellationRequested`），其他一概不行（depguard `bc-isolation-*`）。
- **event 的 schema 歸 consumer 所有**：缺貨是 ordering 的 message（`OrderCancellationRequested`），inventory 把它發進 ordering 的 topic / 表（同一個 DB、同一個交易）。
- **缺貨是狀態不是 error**：`reservation.Reserve` 回 `REJECTED` + `Rejected` event，一樣落地；重送的 `OrderCreated` 找到既有 reservation 就什麼都不做。
  consumer 把 `kernel.Error` 當 final（drop），所以缺貨若用 error 表達就不會有人發取消。
- Stock 與 Order 都用 `version` 做 optimistic lock，輸的那邊拿 `stock.ErrStale` / `order.ErrStale`。它們 wrap `kernel.ErrConcurrentUpdate`，
  **刻意不是** `kernel.Error`：consumer 會 retry 而不是 drop，rpc 由 `server.ToConnectError` 回 `Aborted`（`x-error-code: concurrent_update`），
  `usecasex` 記成 `kind=aborted`、log 為 Warn。兩者的 `Save` 都拆成 version 0 → `INSERT`（存成 1）與
  `UPDATE ... WHERE version = <loaded>`（+1），所以 version 是存檔次數，記憶體中的新 aggregate 永遠對不上已存的 row。
  撞 key 時 Order 回 `order.ErrDuplicate`（id 是 server 產的，撞到就是真的重複），Stock 回 `stock.ErrStale`
  （兩個 SetStock 同時建立同一個 SKU，輸的那個 reload 後改成 restock）。
  兩個同時的 CancelOrder：先 commit 的贏，另一個 `ErrStale`；consumer 重試時 reload 看到 CANCELLED → `ErrCannotCancel` → drop。
- **topic 之間沒有順序保證，用 tombstone 補**：`OrderCancelled` 若比 `OrderCreated` 先到 inventory，`ReleaseStock` 找不到 reservation 時寫一筆
  `CANCELLED`（`reservation.Tombstone`，唯一沒有 lines 的 reservation，不發 event）；晚到的 `OrderCreated` 找到它就什麼都不做。
  兩邊同時 `Add` 撞 primary key 時，輸的那方回 `kernel.ErrConcurrentUpdate`（不是 `kernel.Error`）讓 consumer retry，重試後落在「已存在」分支：
  release 輸給 reserve → 重試時釋放；reserve 輸給 tombstone → 重試時什麼都不做。RESERVED 以外的狀態都是終態。

## Observability：OTel → LGTM

`platform/otel.New(cfg.OTel, cfg.Log)` 回傳 `*otel.Providers`（TracerProvider、MeterProvider、Propagator、root `*slog.Logger`、
`/api/metrics` handler），不碰 global。程式碼只用 OTel API instrument，Prometheus 只是輸出格式：

- **metrics 永遠開**：MeterProvider 掛一個 Prometheus reader，`server` 把它放在 `GET /api/metrics`（沒 collector 也能 scrape）。
  `usecasex.Metrics` 記 `usecase.duration`（exposition：`usecase_duration_seconds{usecase, kind}`），
  `otelconnect` 產 `rpc.server.call.duration`，`contrib/instrumentation/runtime` 產 Go runtime metrics，
  auth interceptor 記 `auth.decisions`（`auth_decisions_total{result, method}`：`ok` / `public` / `anonymous` / `missing` / `invalid` × `api_key` / `jwt` / `none`——
  RPC status 只會說 `unauthenticated`，分不出「沒帶」和「帶了被拒」；label 只有這兩個有限集合，沒有 subject）
- **worker 的 backlog**（`platform/eventbus`，`apps/worker` 註冊）：`eventbus.consumer.duration{topic, result}` 每次 handler
  attempt 一個 sample（retry 也算，被 park 的訊息就是它最後一次 `failed`）；`eventbus.Lag` 掛 observable gauge，每次 collect
  才去查：`eventbus_consumer_lag{topic, consumer_group, broker}` = 該 group 還沒 ack 的訊息數——Postgres 用和 watermill-sql
  subscriber 一樣的謂詞（cursor 之後、且 `transaction_id < snapshot xmin`，所以只算「現在真的讀得到」的），Redis 是 XINFO 的
  `lag + pending`；`eventbus_consumer_oldest_age_seconds` 是最老那筆等了多久（在 SQL / 用 Redis server 的時鐘算，不吃本機時區與時鐘偏移）；
  `eventbus_poison_messages{topic}` 是 `<bc>.poison` 表的 row 數（沒人消費，所以也是累計）。redis 模式多一組
  `consumer_group="forwarder"` 的 Postgres cursor（outbox → stream 那一跳）。有 DB 時同一個 callback 再報 retention 狀態：
  `eventbus_outbox_rows{table}` / `eventbus_outbox_oldest_age_seconds{table}` 是每張 outbox 表的大小與最老一列的年齡，
  `eventbus_outbox_held{table}` / `eventbus_outbox_oldest_held_age_seconds{table}` 是 offsets 表裡**任一** group 還沒 ack 的列
  （prune 不管多老都留著；held 的年齡一直長但 consumer lag 很低 = 有個沒人跑的 stale group），`eventbus_inbox_rows{schema}` /
  `eventbus_inbox_oldest_age_seconds{schema}` 是 `<bc>.inbox`。這些是 `count(*)` 全表掃，表很大時把 OTLP 的 collect 間隔拉長。
  每個 replica 都回報同一組數字：**用 `max`，不要 `sum`**。
  worker 沒有 HTTP server，這些只走 OTLP（60s 一次）
- **有 `otel.endpoint` 才推 OTLP**：traces 走 batcher、metrics 多一個 periodic reader、logs 走 batch processor；`otelconnect`
  interceptor 掛最外層，每個 RPC 一個 span。endpoint 空 = tracer no-op、logger 只寫 stdout、不 dial
- **span 的來源**：RPC 由 `otelconnect`；`/api/docs`、`/api/openapi/*` 等普通 route 由 `otelgin`（filter 掉 RPC prefix、
  `/api/healthz`、`/api/readyz`、`/api/metrics`，不會重複）；每條 SQL 由 `platform/postgres` 自己的小 gorm plugin 掛在 request span 底下（只有 SQL 形狀，不帶變數；不用
  `gorm.io/plugin/opentelemetry`，它會把 ClickHouse / MySQL driver 拖進 binary）
- **logs**：root logger 永遠寫 stdout（`log.format` / `log.level`），有 endpoint 時再 fan-out 一份給 `otelslog` bridge → Loki，
  同一個 level gate。`contextx.Contextx` 的 `Info/Warn/…` 會把 context 帶給 handler，所以 bridge 自動附 `trace_id` / `span_id`
  （stdout 那份靠 interceptor 的 `trace_id` attr）。Loki 裡 attr 是 structured metadata：查 `{service_name="rpc"} | trace_id="…"`，
  不要用 `|=` line filter（比不到）
- **resource**：明確設 `service.name`（= Prometheus `job`）與 `service.instance.id`（= `instance`）——OTLP→Prometheus
  只有這兩個 attribute 會變成 series label，少了 instance.id 多 replica 會寫同一條 series、counter 亂跳

本機：`task compose-up` 連 `grafana/otel-lgtm` 一起起（Grafana `http://localhost:3000`，匿名 admin；OTLP gRPC :4317），
`configs/local.yaml` 的 `otel.endpoint` 已指向它。

## usecasex：use case 的統一形狀與 decorator

```go
type UseCase[I, O any] interface { Execute(c context.Context, in I) (O, error) }
```

adapter 只依賴 `UseCase[I, O]`，不依賴具體 struct。`provider.go` 的 `decorate()` 用 `usecasex.Chain` 把
metrics + logging 包在每個 usecase 外面，rpc 和 consumer 兩邊自動都有。寫入的 usecase 改用 `decorateTx()`，多包一層
transaction。Connect
interceptor 只管 transport 層的事（procedure、protocol、code）。

- `usecasex.Logging`：一行 log。成功是 `Debug`（rpc 路徑已有 interceptor 的 `rpc` access log，成功次數看 metrics），domain error 是 `Info`（預期結果），
  optimistic-lock 衝突是 `Warn`，其他 error 才是 `Error`
- `usecasex.Metrics`：OTel histogram `usecase.duration`（Prometheus：`usecase_duration_seconds{usecase, kind}`）；`kind` =
  `ok` / kernel error kind / `internal`，和 `server.ToConnectError` 同一套判準，attribute 值域有限。MeterProvider 由
  `platform/otel.ProviderSet` 注入（每個 `Initialize()` 一個，不用 global）
- `usecasex.Transactional`：把 usecase 整個放進 `Transactor.Do`。usecase 拿到的 `c` 帶著 tx，repository 與 outbox
  publisher 用 `platformpg.TxOrDB` 取用，所以全部寫入一起 commit / rollback。任何 `Do` 的錯誤（含 commit 失敗）都丟掉 output。
  放在最內層，commit 失敗會被 logging / metrics 記成這次 usecase 的失敗。只給寫入用：讀取包 transaction 只會多一次 BEGIN/COMMIT。
  它保證的是 atomicity，不是 isolation：Postgres 預設 READ COMMITTED，兩個同時的 CancelOrder 都可能讀到 PENDING。
  擋住重複寫入的是 aggregate 的 `version`（見上方 optimistic lock），不是 transaction

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build、helm（`task helm-check`）、helm-kind（`task helm-kind`：kind 真裝 + smoke）、k6（`task k6-smoke`：load / rate limit / timeout / reservation（rpc + worker）四個 k6 腳本打真的 rpc + Postgres）；`buf breaking` **只警告** | 全 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go bump 會讓其他 module 漂移，`dependabot-sync.yml` 在它的分支跑 `task tidy` 補 commit 再觸發 CI，否則 `work-sync` 會擋。`.github/dependabot.yml` 三組每週各一支 PR：gomod（所有 module 一組）、github-actions、npm（`web/` pnpm workspace 一組；lockfile 算 releasable，merge 會切 patch 版）

## Architecture rules：誰擋什麼

| 規則 | 類型 | 擋的工具 |
|---|---|---|
| domain 只 import kernel；application 不碰 infra / adapters / contract；infra 不碰 adapters；adapters 不碰 infra | Clean Arch 依賴方向 | **depguard**（`.golangci.yml`，per-BC 列出；`pkg` 是 prefix match，不能用 `*`） |
| BC 只能從 adapters import 其他 BC 的 `contract/`；kernel / platform 不 import BC；apps 只碰 provider.go | 邊界 | **depguard**（`bc-isolation-<bc>` block） |
| domain struct 欄位 private；無 `Set*`；每個 struct 有 constructor（`*Params` 參數物件除外）；不呼叫 `time.Now()`；不 log | DDD aggregate | **archtest**（`pkgs/platform/archtest`，每個 BC 的 `arch_test.go` 跑） |
| `*Repository` interface 只能在 domain | DDD | archtest |
| usecase 只有 `Execute(c, in) (out, error)` 一個公開方法 | 單一職責、可被 decorator 包 | archtest |
| dto 無方法、不 import domain / proto；port 只有 interface | 邊界純度 | archtest |
| adapter 持有 `usecasex.UseCase[I,O]`，不持有 `*usecase.X` | decorator 生效 | archtest |
| generated code 和 source 一致；go.mod 無漂移 | 一致性 | CI `gen-freshness`、`work-sync` |
| domain 100% coverage | 測試 | `cover-gate.sh` |
| request 形狀（必填、範圍、格式）在 proto 宣告、進 use case 前就擋 | 邊界 | **protovalidate**（`platform/server/validate.go`） |

archtest 只看 AST，零依賴，毫秒級；規則表在 `archtest.DefaultRules()`，BC 可用 `RunRules` 自選。

## OpenAPI

proto-first：OpenAPI 是 proto 的產物，不是另一份契約。`task gen` 用 `protoc-gen-connect-openapi`
（懂 Connect 的路徑與 error shape）把每個 BC 的 spec 生到 `pkgs/<bc>/contract/v1/openapi.yaml`，
`protos/<bc>/v1/openapi.base.yaml` 提供 title / description 等手寫 metadata。
BC 用 `contract/v1/openapi.go` embed 它；`apps/rpc` 的 `NewDocs` 收集各 BC，`APP_HTTP_DOCS=true`
時才掛 `/api/docs`（Scalar UI，`/api/docs/<bc>`）與 `/api/openapi/<bc>.yaml`。production 不開。
spec 裡的 `servers` 是相對的 `/api`（任何 host 都對），但 serve 出去的 yaml 會把它改寫成 request 的絕對 origin（`X-Forwarded-Proto` / Host）：
Scalar 的 Send request 用 `new URL(server + path)`，相對 server 會噴「The URL seems to be invalid」；順便讓檔案直接匯入 Postman / Bruno 就能打。
三份 `openapi.base.yaml` 都宣告 `securitySchemes`（`apiKey` = `X-API-Key`、`bearer` = JWT），所以 Scalar 右上 **Auth** 可以選一種填進去
（`auth.mode: required` 時填 `configs/local.yaml` 的 `auth.api_keys` 那把 key）；`auth.mode: off` 時填不填都一樣。前端不需要它 —— `web/packages/proto`
直接從 proto 生 connect-es client。

## Web console：`web/apps/console`

一個最小的操作介面，用來證明 `@repo/proto` 真的能被前端用：建單、取消、設庫存 / 看全部 SKU，並看 inventory 非同步寫出的
reservation 狀態。

```bash
task run            # rpc :8080（要看到 reservation 還需要 Postgres + task migrate -- up + task worker）
task web-dev        # http://localhost:5173；CONSOLE_API=http://host:port 可改 proxy 目標
task web-typecheck  # @repo/proto + @repo/console 的 tsc --noEmit（CI 的 web job 也跑這個 + web-build）
task web-e2e        # Playwright smoke：建好的 console（vite preview）打真的 ./bin/rpc；orders / stock / poison / prune 各頁走一遍。E2E_DATABASE_URL=postgres://… 先 migrate up 再接那個 DB，並多起一個 bin/worker（CI 的 console-e2e job 這樣跑：poison / prune 真的執行，建單後 Reservation 欄真的變 RESERVED、Cancel 後 RELEASED），沒設就 in-memory、admin 頁驗 admin.no_database；另一組 project auth 起 auth.mode=required（API key + HS256 JWT）的 rpc 驗 401 / 403 / 設 key 或 JWT 恢復、過期 / 錯 issuer 的 token、roles claim 過 admin gate、寫入也帶 credential。第一次要 cd web/apps/console && pnpm exec playwright install chromium（CI 的兩個 console job 走 .github/actions/playwright：chromium 以 @playwright/test 版本為 key 快取在 ~/.cache/ms-playwright，命中只裝 apt deps）
task web-e2e-image  # 同一套 chromium spec 改打真的 console image（e2e/image.sh：build web/apps/console/Dockerfile、容器的 RPC_UPSTREAM 回指 host 上 Playwright 起的 bin/rpc；docker 或 podman），外加 e2e/image.spec.ts 驗 nginx 本身：deep link 回 index.html + no-cache、assets immutable、不存在的 asset 是 404、/api 經 proxy、/healthz。CI 的 console-image job
```

| 主題 | 做法 |
|---|---|
| 呼叫後端 | `@connectrpc/connect-web` + `@repo/proto` 的 service descriptor；vite dev server 把 `/ordering.v1.*`、`/inventory.v1.*` proxy 到 Go server，瀏覽器維持 same-origin，所以 server **不需要 CORS** |
| 錯誤 | `describe()` 讀 `x-error-code` header，並解析 protovalidate 的 `Violations` detail 成 `items[0].sku: …`；`UNAUTHENTICATED` 另外提示去設 credential |
| 認證 | header 右側的 **Credential**：API key（`X-API-Key`）或 JWT（`Authorization: Bearer`），存 localStorage（`console.credential`），transport 的 interceptor 每次呼叫時讀，改了下一個 RPC 就生效；`VITE_CONSOLE_API_KEY` / `VITE_CONSOLE_TOKEN` 可在 build 時 seed。server `auth.mode: off` 時什麼都不用設 |
| 資料 | TanStack Query：server 只有 unary RPC，所以 orders 與 reservation 用 `refetchInterval` polling；reservation 到 REJECTED / RELEASED（終態）就停。domain error 不 retry |
| 路由 | TanStack Router（code-based，`/orders`、`/orders/$id`、`/reservations`、`/stock`、`/poison`、`/prune`），不需要額外的 codegen plugin；`/orders` 的 `status` / `customer` 與 `/reservations` 的 `status` / `order` 走 search params（`validateSearch`），連結可分享；`/orders/$id` 是單筆訂單（每條 line、total、Cancel、inventory 的 reservation），list 的 id 欄與 Reservations 頁的 order 欄都連過去，不存在的 id 顯示 not found 而不是錯誤 |
| Orders 頁 | `ordering.v1.OrderService/ListOrders`（keyset，`created_at desc, id desc`，customer / status filter 綁在 page token 裡）的表格，`useInfiniteQuery` + Load more，每 2s poll；mutation 以 `["orders"]` 前綴 invalidate 所有 filter 的頁 |
| Reservations 頁 | `inventory.v1.InventoryService/ListReservations`（keyset，`created_at desc, order_id desc`，status filter 綁在 page token 裡）的表格，`useInfiniteQuery` + Load more，每 2s poll；Orders 頁的 reservation badge 連到 `/reservations?order=<id>`，該筆釘在最上面 |
| Stock 頁 | `inventory.v1.InventoryService/ListStocks`（keyset，SKU 升冪，opaque page token；`sku_prefix` / `only_reserved` 篩選，token 帶著篩選條件，換條件續頁是 INVALID_ARGUMENT）的表格，`useInfiniteQuery` + Load more，每 2s poll 已載入的頁；篩選走 `/stock?prefix=&reserved=` search params；上方 Set stock 建立 / 覆寫 on hand，列上 Edit 把該 SKU 帶進表單 |
| Poison 頁 | `admin.v1.AdminService`：各 BC 的 poison 表，Show（metadata + payload base64）/ Requeue / Drop，每個 BC 有 Requeue all / Drop all；server auth 開著時 credential 要帶 `admin` role |
| UI | Tailwind v4（`@tailwindcss/vite`）+ shadcn/ui（`components.json`；component 在 `src/components/ui`，加新的用 `pnpm dlx shadcn@latest add <name>`） |

`@repo/proto` 的 `exports` 指向 `src/gen/*.ts`，由 bundler 直接吃 TS 原始檔，不需要 build step。

部署：`web/apps/console/Dockerfile` 把 `task web-build` 的 `dist/` 放進 `nginxinc/nginx-unprivileged`（:8080），`deploy/default.conf.template`
一個 server：`/` 走 SPA fallback（任何路由都回 `index.html`，`assets/` immutable cache）、`/api/` proxy 到 `RPC_UPSTREAM`（預設 `http://rpc:8080`），
瀏覽器維持 same-origin，rpc 一樣不需要 CORS；`/healthz` 給 probe。nginx 自己回的東西（index.html、assets）帶 `deploy/security-headers.conf`：CSP（script 只准 self、style 允許 inline——Radix 用 style attribute、connect 只准 self、`frame-ancestors 'none'`）、nosniff、`X-Frame-Options: DENY`、Referrer-Policy、Permissions-Policy；`/api/` 的 response 是 rpc 的，不加（`/api/docs` 的 Scalar 從 CDN 載）。image 只寫 `/tmp`（entrypoint 用 envsubst 把 template render 到 `/tmp/default.conf`，
`conf.d` 只剩一行 include），所以 read-only rootfs + 任意 uid 都能跑——Helm chart 的 `console.enabled` 就是這樣部署它（見 Deploy）。
release pipeline 把它和三個 Go image 一起推成 `ghcr.io/blackhorseya/x-monorepo/console:X.Y.Z`（見 Release；`task helm-kind` 則從 working tree
build 成 `localhost/x-monorepo/console:kind`）。所以 console 的改動**會切版**：只有 `e2e/` 和 `playwright.config.ts` 不算。

## Conventions

- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`
- Interface 參數 `c context.Context`；實作內 `ctx := contextx.FromContext(c)`
- Domain error 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 code，`Code` 放
  `x-error-code` header；consumer 把 domain error 視為 drop（不重試），infra error 才 retry
- `wire.go` 是 AppConfig → component config 的唯一 mapping 點
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- goimports local-prefix `github.com/blackhorseya/x-monorepo`

## Persistence：Postgres、gorm、migrations

- `platform/postgres.NewDB` 提供 `*gorm.DB`（driver 底層是 pgx，`TranslateError` 開啟）。`APP_POSTGRES_URL` 空 → 回 nil，
  各 BC 的 `Provide*Repository` 看到 nil 就改用 `infra/memory`，所以 `task run`、E2E test 不需要 DB。
- **每個 BC 一個 Postgres schema**（`ordering.orders`、`inventory.stocks`），版本表也在該 schema 裡（`ordering.schema_migrations`）。
  多個 BC 共用一個 database 時，預設的單一 `public.schema_migrations` 會讓兩條 migration 序列互相覆寫版本號且不報錯；
  schema 隔離同時解決表名與版本表的衝突。
- SQL 放 `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql`，由 BC `embed`（go.work 下 embed 不能跨
  module）；`apps/migrate` 只收集各 BC 的 `Migration()` 並用 golang-migrate 執行（cobra：`up` / `down` / `version`，`--help` 有說明；跟 rpc / worker 一樣是同一個 image 的不同 binary）。**service 啟動不跑 migration**，
  部署流程是 `migrate up` → rollout。
- row model（`orderRow`）是持久化模型，不是 aggregate；timestamps 來自 domain 的 `port.Clock`，所以 gorm 的
  `autoCreateTime/autoUpdateTime` 關閉。`total` 不落地，`Reconstitute` 重算。
- driver error 在 `infra/postgres.translate` 轉成 domain error：`gorm.ErrRecordNotFound → order.ErrNotFound`、
  `gorm.ErrDuplicatedKey → order.ErrDuplicate`，其他保持 wrapped 給 log。
- integration test（`infra/postgres`、`provider_test.go`、`apps/migrate`、`apps/worker`）用 `pkgs/platform/postgres/pgtest`：
  `TEST_DATABASE_URL` 沒設就以 testcontainers 起一個 `postgres:17-alpine`（每個 test binary 一個，`TestMain` 結束時 terminate），
  設了就直接用那個 DB（連不上是 FAIL，不是 skip）。只有「本機沒有 container runtime」會 skip，且 CI 裡一律 FAIL。
  Podman 使用者要在 `~/.testcontainers.properties` 放 `ryuk.disabled=true`：Ryuk 寫死用名為 `bridge` 的 network，Podman 沒有。

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
| Eventbus consumer lag | `eventbus_consumer_lag` > 100 持續 5m | handler 追不上或沒人在消費；看 oldest age 分辨，再加 worker replica |
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

## Release（goreleaser + release-drafter）

**每次 CI 在 main 上綠就是一版**，沒有人手動打 tag（`.github/workflows/release.yml`，`workflow_run` 掛在 ci 後面；
`workflow_dispatch` 可對目前 main HEAD 重跑）。`workflow_run` 沒有 `paths-ignore`，所以 prepare 先自己 gate：拿該 commit 和
**上一個 tag** 做 diff（不是 parent commit——ci 對 main 是 cancel-in-progress，連續兩次 merge 只會觸發一次 release），
沒有任何檔案會進到 archive / image 就整條跳過（排除清單在 `scripts/releasable.sh`：docs、CI / hook / lint / `Taskfile.yml`、
`scripts/`、Helm chart、kind / compose 開發環境、tests 與 test-only 套件（含 console 的 `e2e/`）；`web/` 其餘都算，console image 是 artifact；
step summary 會列出原因；archive 裡的 README 會停在上一版，可接受）。
`task release-gate` 在本機跑同一個腳本，merge 前就知道會不會發版，會的話再跑 `scripts/next-version.sh` 從 commit title 算 drafter 會給的版號（`!` → major、`feat` → minor、其餘 patch；main 上每個 commit 都是 squash 進來的 PR title，所以和 drafter 一致）。
每個 PR 上另有一則 sticky comment 說同一件事（`.github/workflows/release-gate.yml` → `scripts/release-gate-pr.sh`：「merge 會發 vX.Y.Z（minor — feat: …）」或「不發版」、這個 PR 哪些檔會進 artifact、main 上從上一個 tag 起已經累積了什麼；版號用 PR title 當 squash commit 的 subject 算，retitle 會重算；純建議，不擋 PR，fork PR 不貼）。`workflow_dispatch` 不過 gate：

| job | 做什麼 |
|---|---|
| prepare | gate 過了才跑 release-drafter：依 merge 進來的 PR title 分類（`when.conventional`，不用 label）產 draft release `vX.Y.Z`；版號：`!` → major、`feat` → minor、其餘 patch，`test:` 不進 notes |
| （labeler.yml） | 不在 release 鏈上：`pull_request_target` 跑 release-drafter 的 autolabeler，依 title 的 type 貼 `feature` / `fix` / `refactor` / `test` / `documentation` / `ci` / `breaking`，依路徑貼 `proto` / `web` / `helm` / `ordering` / `inventory` / `platform`。純顯示，版號不看它 |
| build | 在該 commit 本地打 tag（不 push，goreleaser 需要）→ `task web-build` → `goreleaser release`：archives 掛上那份 draft（`use_existing_draft` + `keep-existing`，notes 保留）、三個 multi-arch image 推 `ghcr.io/blackhorseya/x-monorepo/{rpc,worker,migrate}:X.Y.Z`（非 prerelease 另打 `latest`）→ `docker buildx build --push` 把 console 的 `dist/` 推成第四個 image `.../console:X.Y.Z`（同一套 tag 規則；goreleaser 沒有 binary 可以掛它）。四個 image 都附 SBOM（SPDX）+ SLSA provenance attestation，並由 cosign keyless 以 digest 簽名（job 有 `id-token: write`，certificate 的 identity 是這個 workflow）；`checksums.txt` 也簽（cosign v3 的 Sigstore bundle `checksums.txt.sigstore.json` 掛上 release，簽章 + certificate + Rekor entry 一個檔） |
| publish | `gh release edit --draft=false --target <sha>`：GitHub 建 tag、標 latest。image 已經在 registry，release 一出現就能拉 |

```bash
task release-check      # 驗 .goreleaser.yaml
task release-snapshot   # 本機 dry run：每個 app × linux/darwin × amd64/arm64 → bin/dist，不做 image、不簽名、不 publish
```

**驗證**（release notes 的 Verify 區塊也有同一組指令；image 要先 `docker login ghcr.io`，package 是 private）：

```bash
ID='https://github.com/blackhorseya/x-monorepo/.github/workflows/release.yml@refs/heads/main'
ISS=https://token.actions.githubusercontent.com
cosign verify --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" ghcr.io/blackhorseya/x-monorepo/rpc:X.Y.Z
cosign verify-blob --certificate-identity "$ID" --certificate-oidc-issuer "$ISS" --bundle checksums.txt.sigstore.json checksums.txt
docker buildx imagetools inspect ghcr.io/blackhorseya/x-monorepo/rpc:X.Y.Z --format '{{ json .SBOM }}'   # 或 .Provenance
```

`deployments/docker/Dockerfile` 只 COPY 預先 cross-compile 好的 binary 到 `distroless/static:nonroot`（有 CA certs、無 shell；
healthcheck 從外面打 `/api/readyz`）。兩個 binary 都吃 SIGTERM 做 graceful shutdown（rpc：readyz 先 503 → drain，見 server 一節；worker：in-flight handler 跑完才停，見 Events 一節），
部署的 grace period 要大於 `http.shutdown_delay + http.shutdown_timeout`（rpc）/ 18s（worker：8s drain + 10s router close）——Helm chart 把這兩個數字算死（下一節）。Binary 用 `-X` 把版本寫進 `pkgs/platform/buildinfo`，OTel resource 的 `service.version` 就是它；三個 binary 都吃 `--version`，rpc 的 `server listening` 與 worker 的 `worker started` log 帶 `version` / `commit`，rpc 另有 `GET /api/version`
（`go build` 直接跑的是 `dev`）。CI 的 `release-snapshot` job 在每個 PR 跑一次 snapshot，壞掉的 release 設定在 merge 前就會紅，release 的 build job 也共用它的 Go cache（`cache-scope: release-snapshot`），cross-compile 不用冷啟。
用 `GITHUB_TOKEN` publish 的 release 不會觸發其他 workflow 的 `push: tags` / `release` event（GitHub 防遞迴），要接部署就像
release.yml 那樣用 `gh workflow run` 明確 dispatch。第一次 push 到 ghcr 的 package 是 private，要到 GitHub → Packages 連到 repo / 改 public 一次。

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
上面各節講的「部署要配合的行為」寫成算式，而不是讓人手抄：

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
  --set-string secrets.postgresUrl='postgres://…' --set config.otel.endpoint=otel-collector:4317 \
  --set 'imagePullSecrets[0].name=ghcr'    # ghcr package 是 private
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
| `auth.mode` | `APP_AUTH_MODE` | `off` | `off` 不掛 auth interceptor；`optional` 有帶 credential 才驗（沒帶 = anonymous，帶了驗不過一樣是錯）；`required` 沒有合法 credential → `UNAUTHENTICATED` |
| `auth.api_keys` | `APP_AUTH_API_KEYS` | 空 | `name=key[:role\|role],name=key`（key ≥ 16 字元），對 `X-API-Key` header；principal 的 subject = name、roles = 冒號後的清單。一個字串，所以 env 帶得動 |
| `auth.admin_role` | `APP_AUTH_ADMIN_ROLE` | `admin` | `admin.v1.AdminService`（poison 操作）要求的 role；`auth.mode: off` 時不檢查 |
| `auth.jwt.hs256_secret` / `auth.jwt.public_key` | `APP_AUTH_JWT_HS256_SECRET` / `APP_AUTH_JWT_PUBLIC_KEY` | 空 | 二選一：HS256 secret（≥ 32 bytes）或 PEM public key（RSA / EC / Ed25519，演算法家族釘死在 key 型別上）。對 `Authorization: Bearer <jwt>`，`exp` 必填 |
| `auth.jwt.issuer` / `auth.jwt.audience` | `APP_AUTH_JWT_ISSUER` / `APP_AUTH_JWT_AUDIENCE` | 空 | 設了就檢查 `iss` / `aud` |
| `auth.public` | `APP_AUTH_PUBLIC` | 空 | 不需 credential 的 procedure 清單（`/ordering.v1.OrderService/GetOrder`，env 用逗號分隔） |
| `postgres.url` | `APP_POSTGRES_URL` | 空 | 空 = in-memory；`apps/migrate` 必填 |
| `otel.endpoint` | `APP_OTEL_ENDPOINT` | 空 | OTLP/gRPC `host:port`；空 = telemetry 關 |
| `otel.insecure` | `APP_OTEL_INSECURE` | `false` | plaintext gRPC（本機 collector） |
| `otel.service_name` | `APP_OTEL_SERVICE_NAME` | 空 | `service.name`；空 = 執行檔名 |
| `worker.consumer_group` | `APP_WORKER_CONSUMER_GROUP` | `worker` | `apps/worker` 的 offset cursor 前綴，實際 group 是 `<值>.<bc>`（每個 BC 各自讀完整個 topic）；同名 replica 分工，新名字從哪開始看 `worker.start_from` |
| `worker.start_from` | `APP_WORKER_START_FROM` | `beginning` | **還沒有 cursor** 的 group 從哪開始：`beginning` 重播所有留存的訊息，`latest` 只收建立之後的。已有 cursor 的 group 不會被移動；redis 模式也套用到 forwarder 的 SQL group 與 stream group |
| — | `APP_CONFIG` | 空 | 指定 config 檔路徑（Taskfile 的 `CONFIG` var 會設它） |
| `redis.url` | `APP_REDIS_URL` | 空 | 只在 `worker.broker: redis` 時連（啟動 ping）；預設 broker 下 worker 完全不碰它 |
| `worker.broker` | `APP_WORKER_BROKER` | `postgres` | `postgres` 直接讀 outbox 表；`redis` 由 worker 轉送進 Redis Streams 再讀 |
| `worker.handler_timeout` | `APP_WORKER_HANDLER_TIMEOUT` | `10s` | 單次 handler attempt 的 deadline（`http.timeout` 的 consumer 版）：usecase 的 context 到期、該次算 failed 照常 retry，用完後以 `handler timed out` 為 reason 進 poison；`0` = 不限。SIGTERM 時 in-flight handler 的上限是它和 drain 的 8s 取短 |
| — | `TEST_DATABASE_URL` | 未設 | 只給 integration test：設了就用該 DB，未設則 testcontainers 自起 |
| — | `TEST_REDIS_URL` | 未設 | 同上，給 worker E2E 的 redis 模式 |

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto`，request 欄位的形狀規則（必填 / 範圍 / pattern）用 `buf.validate` 寫在 message 上，不要在 adapter 手刻
   → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`，讀取用 `decorate(name, rec, uc)`，寫入用 `decorateTx(name, rec, tx, uc)`
4. `adapters/rpc` 實作新方法（`var _ XxxServiceHandler` 會提醒）→ `task wire`

### 新增 Bounded Context（`pkgs/inventory` 是照這個清單加的第二個 BC，可當範本）
1. `protos/<newbc>/v1/{entity,service,event}.proto` + `openapi.base.yaml`；`buf.gen.yaml` 加一行 `go_package` override
2. `pkgs/<newbc>/go.mod`（module path 對齊目錄，`replace` 指向 kernel / platform / 要讀 contract 的 BC），`go.work` 加 `use`，
   `Taskfile.yml` 的 `MODULES` 與 `BCS` 加上，`apps/*/go.mod` 加 `require` + `replace` → `task tidy`
3. `contract/v1/openapi.go`（embed）→ `task gen`；複製 `pkgs/ordering` 結構，寫 `provider.go`（`Subscriptions` 用自己的 named type）與 `arch_test.go`
4. `.golangci.yml`：複製 `ordering-*` 四個 layer block、加 `bc-isolation-<newbc>`，並把新 BC 加進**每個**既有 BC 的 isolation / layer deny
   以及 `shared` / `apps` / `apps-migrate`（`pkg` 是 prefix match，不能用 `*`）
5. `apps/rpc`：`NewRoutes` / `NewDocs` 加參數、`wire.go` 加 `ProviderSet`；`apps/worker`：`wire.go` 加 `ProviderSet`、`NewWorker` 收它的
   `Subscriptions` 並 `Register`（自己的 poison topic）→ `task wire`
6. 有 DB 的話：`infra/postgres/migrations/0001_*.sql`（表都放在 `<newbc>.` schema）+ 自己發的 topic 與 `<newbc>.poison` 的 events 表 + `Migration()`，
   `apps/migrate/main.go` 的 `migrations` 加一筆（依賴順序：發進別人表的排後面）

### 新增 consumer / 新 event
- **發**：domain 加 event struct（embed `base`）並在行為裡 `record()`；`protos/<bc>/v1/event.proto` 加 message → `task gen`；
  `adapters/events.toProto` 加 case；migration 加 `events_<topic>` + `_offsets` 兩張表（照 `0003_events.up.sql`）。
- **收**：`adapters/consumer` 加 handler（error contract 見 `handler.go`）→ `provider.go` 的 `Subscriptions` 加一筆；
  訂閱別的 BC 的 topic 時，表由發的那個 BC 的 migration 建，consumer 只 import 它的 `contract/`。`apps/worker` 不用改，除非是新 BC（見上）。
- 要在同一個 process 跑 rpc + consumer 也可以（go-micro 式），那是 `apps/` 的決定，BC 不在乎。

### 拆 BC 成獨立服務
1. 對 `pkgs/kernel`、`pkgs/platform` 打 tag：`pkgs/kernel/v0.1.0`、`pkgs/platform/v0.1.0`
2. `git mv pkgs/<bc>` 到新 repo，`go.mod` 的 `require` 改成 tag 版本
3. `protos/<bc>` 跟著走（或進 BSR）；新 repo 加 `apps/rpc`
4. 原 repo 的 `go.work`、`Taskfile.yml`、`.golangci.yml`、`apps/rpc/wire.go` 移除該 BC

## Rename module

```bash
grep -rl 'github.com/blackhorseya/x-monorepo' --include='*.go' --include='go.mod' --include='go.work' \
  --include='buf.gen.yaml' --include='.golangci.yml' --include='Taskfile.yml' . \
  | xargs sed -i '' 's#github.com/blackhorseya/x-monorepo#github.com/<org>/<repo>#g'
task gen wire
```
