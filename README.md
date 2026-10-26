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
│   ├── inventory/v1/              # 第二個 BC 的契約（entity / service / event）
│   └── identity/v1/               # 第三個 BC：account / API key（entity / service，沒有 event）
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
│   ├── inventory/                 # ── 第二個 BC：同樣結構 ──
│       ├── domain/stock/          # Stock aggregate（每 SKU 一筆，optimistic version）
│       ├── domain/reservation/    # Reservation aggregate（每 order 一筆：RESERVED / REJECTED / RELEASED）
│       └── adapters/consumer/     # 消費 ordering.v1.OrderCreated / OrderCancelled（只 import ordering 的 contract/）
│   └── identity/                  # ── 第三個 BC：DB 管理的 account + API key，給 auth interceptor 解 X-API-Key（docs/identity.md）──
│       ├── domain/account/        # Account aggregate（id = principal subject、roles、ACTIVE / DISABLED）
│       ├── domain/apikey/         # APIKey aggregate（只存 sha256、prefix；ACTIVE / REVOKED、expires_at）
│       └── adapters/auth/         # 實作 platform 的 server.CredentialResolver（TTL cache），apps/rpc 以 wire.Bind 接上
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

## Docs

README 放架構與程式碼層面的規則；流程、維運與參考表在 `docs/`：

| 檔 | 內容 |
|---|---|
| [`docs/ci.md`](docs/ci.md) | gates：hooks / lefthook / CI 各跑什麼、CI 的 job 與依路徑的 gating、相關 scripts |
| [`docs/release.md`](docs/release.md) | release-drafter + goreleaser 的 release 鏈、release gate、簽名 / attestation 與驗證 |
| [`docs/operations.md`](docs/operations.md) | Compose 本機環境（Grafana dashboards / alerts）、k6 load test、Helm 部署與 kind 實測 |
| [`docs/observability.md`](docs/observability.md) | OTel → LGTM：metrics、traces、logs、每個 metric 與 alert 的意義 |
| [`docs/config.md`](docs/config.md) | 所有 config key、預設值、env 覆寫規則、`auth.*` |
| [`docs/error-codes.md`](docs/error-codes.md) | `x-error-code` 總表 |
| [`docs/workflow.md`](docs/workflow.md) | 新增 RPC / Bounded Context / consumer、拆 BC 成獨立服務、rename module |
| [`docs/identity.md`](docs/identity.md) | identity BC：DB 管理的 account / API key、`server.CredentialResolver`、principal 語意、cache TTL、帳密 / issuer 的擴充路徑 |

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

## Validation：protovalidate 在邊界擋 request 形狀

| 層 | 擋什麼 | 位置 |
|---|---|---|
| proto | `buf.validate` rule 直接寫在 request message 上（`min_len`、`gt`、`gte`、`pattern`、`required`、`min_items`） | `protos/<bc>/v1/service.proto`、`common/v1/money.proto`、`ordering/v1/entity.proto` |
| server | `NewAuthInterceptor`（`auth.mode` 不是 `off` 才掛，在 logging 之內、validate 之外）：`X-API-Key` 先對 `auth.api_keys`（static，break-glass）再問 `server.CredentialResolver`（`pkgs/identity` 的 DB key，見 `docs/identity.md`；resolver 失敗 = 拒絕，不放行；一次查詢受 `auth.resolver_timeout` 限制），否則 `Authorization: Bearer` 對 `auth.jwt`，解析成 `contextx.Principal{Subject, Name, Roles, Method, CredentialID}` 放進 context；`rpc` access line 帶 `auth`（`api_key` / `jwt` / `anonymous` / `public`）與 `sub`（auth 在 logging 之內，透過 context 裡的 slot 回報）、usecase 的 log 帶 `sub`、span 帶 `enduser.id` / `enduser.roles` / `auth.method`——Loki 與 Tempo 都查得到誰呼叫的，被拒絕的呼叫也有一行（`code=unauthenticated auth=anonymous`）；每個決定計入 `auth_decisions_total{result, method}`（dashboard 的 Auth row 與 `rpc-auth` alert 看它）；name 與 credential 本身不記；usecase 用 `contextx.PrincipalFrom(c)` 讀（consumer path / mode off 是 anonymous zero value）。只做 authentication，授權是讀的人的事。沒帶 → `UNAUTHENTICATED` + `x-error-code: auth.unauthenticated`，帶了驗不過 → `auth.invalid_credential`。probes / `/api/metrics` / `/api/docs` 是 gin route，不在 RPC chain 裡，永遠開著 | `pkgs/platform/server/auth.go`、`pkgs/platform/contextx/principal.go` |
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
| apps/worker | 另一個 process：wire 每個 BC 的 `ProviderSet`，把各 BC 的 `Subscriptions`（`ProvideSubscriptions`，named type 讓 wire 分得開）掛上同一個 router；每個 BC 自己的 subscriber / consumer group（`<worker.consumer_group>.<bc>`）與 poison topic。`task worker`，需要 `postgres.url`；同 `worker.consumer_group` 的 replica 分工。`worker poison …`（`task poison -- …`）是 poison 表的操作面、`worker prune`（`task prune -- …`）是 outbox / inbox 的 retention | `apps/worker`；`worker bootstrap`（`task bootstrap`）是 identity 的第一個 account + API key（空 store 才建，secret 印一次；identity 沒 event，只掛這個 injector，不進 router） |

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
  一列只要還有**一個**有 cursor 的 group 沒讀就留著，NOTE 欄列出每個還有未讀列的 group、幾列、最老一列多老——到 retention 的是 `held back by`（真的擋住 prune），還沒到的是 `behind`（live group 兩次 poll 之間也會短暫出現；stale group 不用等到 retention 就看得到，和 held gauge / alert 同步）；不會再跑的 group（對這個 DB 跑過的 E2E 留下的 `test-*`、
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
                                              ├─ RESERVED → inventory.v1.StockReserved ──► ordering ConfirmOrder（PENDING → CONFIRMED → ordering.v1.OrderConfirmed；已 CANCELLED → domain error → drop）
                                              └─ REJECTED → ordering.v1.OrderCancellationRequested ──► ordering CancelOrder
CancelOrder ──► ordering.v1.OrderCancelled ──► inventory ReleaseStock（RESERVED → RELEASED；找不到 → CANCELLED tombstone；否則 domain error → drop）
CompleteOrder ─► ordering.v1.OrderCompleted ──► inventory CommitStock（RESERVED → COMMITTED：on_hand 與 reserved 同時扣掉——賣掉了；否則 domain error → drop）
worker expire ─► inventory ExpireReservations（RESERVED 超過 TTL → EXPIRED：stock 還回去 + inventory.v1.StockReleased，並發 ordering.v1.OrderCancellationRequested ──► ordering CancelOrder（CONFIRMED → CANCELLED）；回來的 OrderCancelled 找到 EXPIRED → drop）
```

Order 的狀態機：PENDING → CONFIRMED | CANCELLED，CONFIRMED → COMPLETED | CANCELLED（確認後客戶仍可取消，inventory 收到 `OrderCancelled` 照常 release）；COMPLETED 是終點，不能再取消。
`ConfirmOrder` 沒有 RPC：確認是 inventory 的答覆，不是 client 的呼叫；`CompleteOrder` 只有 RPC（只有 CONFIRMED 能完成，`order.cannot_complete`）。
cancel 與 complete 同時到不會兩個都成立：`Save` 比 version，輸的那邊 reload 後被狀態機拒絕，所以每張 order 只會有 `OrderCancelled` / `OrderCompleted` 其一。
**Reservation TTL**：客戶既不完成也不取消，庫存不能一直被扣著——`worker expire --older-than 24h`（Helm `expire` CronJob 每 15 分鐘；`task expire -- --older-than 24h --dry-run` 先看）掃 RESERVED 超過 TTL 的 reservation，一批一個交易、最老的先。
Reservation 沒有 version：它和同一張 order 的 Commit / Release 之間的鎖是 **stock 列的 version**（同一交易裡一起 Save），輸的那批整批 rollback 重跑。
已知的窗口：`CompleteOrder` commit 到 inventory 消費 `OrderCompleted` 之間 reservation 仍是 RESERVED，sweep 剛好掃到會把庫存還回去並請求取消，ordering 拒絕（COMPLETED 是終點）、`OrderCompleted` 到了找到 EXPIRED 也 drop——結果是 order COMPLETED 但沒賣出。窗口是一跳 consumer 對上以小時計的 TTL，守的是 eventbus lag 的 alert，不是 use case。

- ordering 與 inventory 各只 import 對方的 `contract/v1`（inventory：consumer 解 `OrderCreated` / `OrderCancelled` / `OrderCompleted`、events adapter 發 `OrderCancellationRequested`（缺貨與過期都是它）；ordering：consumer 解 `StockReserved`），其他一概不行（depguard `bc-isolation-*`）。
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
四份 `openapi.base.yaml` 都宣告 `securitySchemes`（`apiKey` = `X-API-Key`、`bearer` = JWT），所以 Scalar 右上 **Auth** 可以選一種填進去
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
| 路由 | TanStack Router（code-based，`/orders`、`/orders/$id`、`/reservations`、`/stock`、`/poison`、`/prune`），不需要額外的 codegen plugin；`/orders` 的 `status` / `customer` 與 `/reservations` 的 `status` / `order` 走 search params（`validateSearch`），連結可分享；`/orders/$id` 是單筆訂單（每條 line、total、Cancel、inventory 的 reservation），list 的 id 欄與 Reservations 頁的 order 欄都連過去，`/orders` 另有 Order id 輸入框貼完整 id 直接開（log / ticket 抄來的 id 不用在 list 裡翻頁找），不存在的 id 顯示 not found 而不是錯誤 |
| Orders 頁 | `ordering.v1.OrderService/ListOrders`（keyset，`created_at desc, id desc`，customer / status filter 綁在 page token 裡）的表格，`useInfiniteQuery` + Load more，每 2s poll；mutation 以 `["orders"]` 前綴 invalidate 所有 filter 的頁 |
| Reservations 頁 | `inventory.v1.InventoryService/ListReservations`（keyset，`created_at desc, order_id desc`，status filter 綁在 page token 裡）的表格，`useInfiniteQuery` + Load more，每 2s poll；Orders 頁的 reservation badge 連到 `/reservations?order=<id>`，該筆釘在最上面 |
| Stock 頁 | `inventory.v1.InventoryService/ListStocks`（keyset，SKU 升冪，opaque page token；`sku_prefix` / `only_reserved` 篩選，token 帶著篩選條件，換條件續頁是 INVALID_ARGUMENT）的表格，`useInfiniteQuery` + Load more，每 2s poll 已載入的頁；篩選走 `/stock?prefix=&reserved=` search params；上方 Set stock 建立 / 覆寫 on hand，列上 Edit 把該 SKU 帶進表單 |
| Poison 頁 | `admin.v1.AdminService`：各 BC 的 poison 表，Show（metadata + payload base64）/ Requeue / Drop，每個 BC 有 Requeue all / Drop all；server auth 開著時 credential 要帶 `admin` role |
| UI | Tailwind v4（`@tailwindcss/vite`）+ shadcn/ui（`components.json`；component 在 `src/components/ui`，加新的用 `pnpm dlx shadcn@latest add <name>`） |

`@repo/proto` 的 `exports` 指向 `src/gen/*.ts`，由 bundler 直接吃 TS 原始檔，不需要 build step。

部署：`web/apps/console/Dockerfile` 把 `task web-build` 的 `dist/` 放進 `nginxinc/nginx-unprivileged`（:8080），`deploy/default.conf.template`
一個 server：`/` 走 SPA fallback（任何路由都回 `index.html`，`assets/` immutable cache）、`/api/` proxy 到 `RPC_UPSTREAM`（預設 `http://rpc:8080`），
瀏覽器維持 same-origin，rpc 一樣不需要 CORS；`/healthz` 給 probe。nginx 自己回的東西（index.html、assets）帶 `deploy/security-headers.conf`：CSP（script 只准 self、style 允許 inline——Radix 用 style attribute、connect 只准 self、`frame-ancestors 'none'`）、nosniff、`X-Frame-Options: DENY`、Referrer-Policy、Permissions-Policy；`/api/` 的 response 是 rpc 的，不加（`/api/docs` 的 Scalar 從 CDN 載）。image 只寫 `/tmp`（entrypoint 用 envsubst 把 template render 到 `/tmp/default.conf`，
`conf.d` 只剩一行 include），所以 read-only rootfs + 任意 uid 都能跑——Helm chart 的 `console.enabled` 就是這樣部署它（見 [`docs/operations.md`](docs/operations.md)）。
release pipeline 把它和三個 Go image 一起推成 `ghcr.io/blackhorseya/x-monorepo/console:X.Y.Z`（見 [`docs/release.md`](docs/release.md)；`task helm-kind` 則從 working tree
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
