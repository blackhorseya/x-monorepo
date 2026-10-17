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

curl -X POST localhost:8080/ordering.v1.OrderService/CreateOrder \
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
├── deployments/docker/            # docker-compose.yaml：本機 infra（Postgres），app 不進容器
├── apps/
│   ├── rpc/                       # composition root：main + wire，無業務、無 mapping
│   ├── worker/                    # 事件 consumer process：同一份 ProviderSet + Watermill router
│   └── migrate/                   # 收集各 BC 的 embed migrations，up / down / version；唯一能 import infra 的 app
└── web/                           # Turborepo：apps/*、packages/proto（@repo/proto）
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

`platform/server` 用 gin 當 HTTP router：每個 BC 的 Connect handler 以 `gin.WrapH` 掛在 `/<pkg>.<Service>/*procedure`，
`/api/healthz`、`/api/metrics`、`/api/docs` 是普通 gin route（非 RPC 的東西都在 `/api` 下，ingress 一條規則）。`RedirectTrailingSlash` / `RedirectFixedPath` 關閉（gRPC client
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
| application | `port.Transactor.Do` 包住 `repo.Save` + `port.EventPublisher.Publish`：同一個交易 | `usecase/create_order.go`、`cancel_order.go` |
| adapters/events | domain event → `ordering.v1.OrderCreated` / `OrderCancelled` proto bytes → `message.Publisher`；topic = proto full name，metadata 帶 `key`（aggregate id）與 `traceparent` | `adapters/events/publisher.go` |
| platform/eventbus | Watermill：`PostgresPublisher` 從 message context 取出 use case 的 `*sql.Tx` 寫 `<bc>.events_<topic>`（**outbox 就是 commit 本身**）；`NewPostgresSubscriber` + `NewRouter`（Recoverer → tracing）+ `Register`（每個 BC：PoisonQueue → Retry → drop）；`ProvidePublisher` 由 app 提供一次給所有 BC；沒 DB 時用 GoChannel（at most once、同 process） | `pkgs/platform/eventbus` |
| infra | `postgres.Transactor` 把 gorm tx 放進 context，repository / publisher 用 `platformpg.TxOrDB` 拿；memory 版直接呼叫 fn | `infra/postgres/transactor.go` |
| apps/worker | 另一個 process：wire 每個 BC 的 `ProviderSet`，把各 BC 的 `Subscriptions`（`ProvideSubscriptions`，named type 讓 wire 分得開）掛上同一個 router；每個 BC 自己的 subscriber / consumer group（`<worker.consumer_group>.<bc>`）與 poison topic。`task worker`，需要 `postgres.url`；同 `worker.consumer_group` 的 replica 分工 | `apps/worker` |

- Topic → table：`ordering.v1.OrderCreated` → `ordering.events_v1_ordercreated`（+ `_offsets`），第一段就是 BC，**DDL 由該 BC 的 migration 出**（`0003_events`），
  service 不在啟動時建表；`InitializeSchema` 關閉。新 topic = 新 migration。
- 語意：at-least-once。handler 回 `eventbus.ErrDrop`（壞 payload、domain 說不行 → 例如重送的取消要求碰到 `ErrCannotCancel`）= ack 不重試也不進 poison；
  其他 error → Retry（3 次、指數退避）→ `ordering.poison` 表（`events_poison`）。consumer 的冪等由 domain 狀態機保證。
- Trace：publish 時把 `traceparent` 寫進 metadata，router 的 tracing middleware 還原成 CONSUMER span，Tempo 看得到 rpc → publish → handler 一條線。
- 換 broker：`message.Publisher` / `Subscriber` 是 port；換 NATS / Kafka 只動 `provider.go` 的 `ProvidePublisher` 與 worker 的 subscriber，handler 與 error contract 不變
  （SQL 表想保留當 outbox 的話，用 Watermill 的 forwarder component 轉送）。
- 代價：watermill-sql 會把 `lib/pq` 一起連進來（只用它的 error type），實際連線仍走 pgx。

### 跨 BC 的示範：ordering ↔ inventory

```
CreateOrder ──► ordering.v1.OrderCreated ──► inventory ReserveStock（全有或全無）
                                              ├─ RESERVED → inventory.v1.StockReserved
                                              └─ REJECTED → ordering.v1.OrderCancellationRequested ──► ordering CancelOrder
CancelOrder ──► ordering.v1.OrderCancelled ──► inventory ReleaseStock（RESERVED → RELEASED；否則 domain error → drop）
```

- inventory 只 import `ordering/contract/v1`（consumer 解 `OrderCreated`、events adapter 發 `OrderCancellationRequested`），其他一概不行（depguard `bc-isolation-*`）。
- **event 的 schema 歸 consumer 所有**：缺貨是 ordering 的 message（`OrderCancellationRequested`），inventory 把它發進 ordering 的 topic / 表（同一個 DB、同一個交易）。
- **缺貨是狀態不是 error**：`reservation.Reserve` 回 `REJECTED` + `Rejected` event，一樣落地；重送的 `OrderCreated` 找到既有 reservation 就什麼都不做。
  consumer 把 `kernel.Error` 當 final（drop），所以缺貨若用 error 表達就不會有人發取消。
- Stock 用 `version` 做 optimistic lock：`Save` 是 `ON CONFLICT ... WHERE stocks.version = EXCLUDED.version`，輸的那邊拿 `stock.ErrStale`——
  它**刻意不是** `kernel.Error`，consumer 會 retry 而不是 drop。
- 已知限制：topic 之間沒有順序保證。`OrderCancelled` 若比 `OrderCreated` 先到 inventory，release 找不到 reservation（drop），之後的 reserve 會留住庫存；
  要補的話是 reservation 的 tombstone（先收到 cancel 就記一筆 RELEASED），尚未做。

## Observability：OTel → LGTM

`platform/otel.New(cfg.OTel, cfg.Log)` 回傳 `*otel.Providers`（TracerProvider、MeterProvider、Propagator、root `*slog.Logger`、
`/api/metrics` handler），不碰 global。程式碼只用 OTel API instrument，Prometheus 只是輸出格式：

- **metrics 永遠開**：MeterProvider 掛一個 Prometheus reader，`server` 把它放在 `GET /api/metrics`（沒 collector 也能 scrape）。
  `usecasex.Metrics` 記 `usecase.duration`（exposition：`usecase_duration_seconds{usecase, kind}`），
  `otelconnect` 產 `rpc.server.call.duration`，`contrib/instrumentation/runtime` 產 Go runtime metrics
- **有 `otel.endpoint` 才推 OTLP**：traces 走 batcher、metrics 多一個 periodic reader、logs 走 batch processor；`otelconnect`
  interceptor 掛最外層，每個 RPC 一個 span。endpoint 空 = tracer no-op、logger 只寫 stdout、不 dial
- **span 的來源**：RPC 由 `otelconnect`；`/api/docs`、`/api/openapi/*` 等普通 route 由 `otelgin`（filter 掉 RPC prefix、
  `/api/healthz`、`/api/metrics`，不會重複）；每條 SQL 由 `platform/postgres` 自己的小 gorm plugin 掛在 request span 底下（只有 SQL 形狀，不帶變數；不用
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
metrics + logging 包在每個 usecase 外面（之後加 tx 也在這一處），rpc 和 consumer 兩邊自動都有。Connect
interceptor 只管 transport 層的事（procedure、protocol、code）。

- `usecasex.Logging`：一行 log，domain error 是 `Info`（預期結果），其他 error 才是 `Error`
- `usecasex.Metrics`：OTel histogram `usecase.duration`（Prometheus：`usecase_duration_seconds{usecase, kind}`）；`kind` =
  `ok` / kernel error kind / `internal`，和 `server.ToConnectError` 同一套判準，attribute 值域有限。MeterProvider 由
  `platform/otel.ProviderSet` 注入（每個 `Initialize()` 一個，不用 global）

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build；`buf breaking` **只警告** | 全 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go 的 version updates 已關（`.github/dependabot.yml`），security updates 來時在它的分支跑 `task tidy` 再合，否則 `work-sync` 會擋

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

archtest 只看 AST，零依賴，毫秒級；規則表在 `archtest.DefaultRules()`，BC 可用 `RunRules` 自選。

## OpenAPI

proto-first：OpenAPI 是 proto 的產物，不是另一份契約。`task gen` 用 `protoc-gen-connect-openapi`
（懂 Connect 的路徑與 error shape）把每個 BC 的 spec 生到 `pkgs/<bc>/contract/v1/openapi.yaml`，
`protos/<bc>/v1/openapi.base.yaml` 提供 title / description 等手寫 metadata。
BC 用 `contract/v1/openapi.go` embed 它；`apps/rpc` 的 `NewDocs` 收集各 BC，`APP_HTTP_DOCS=true`
時才掛 `/api/docs`（Scalar UI，`/api/docs/<bc>`）與 `/api/openapi/<bc>.yaml`。production 不開。前端不需要它 —— `web/packages/proto`
直接從 proto 生 connect-es client。

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
  module）；`apps/migrate` 只收集各 BC 的 `Migration()` 並用 golang-migrate 執行。**service 啟動不跑 migration**，
  部署流程是 `migrate up` → rollout。
- row model（`orderRow`）是持久化模型，不是 aggregate；timestamps 來自 domain 的 `port.Clock`，所以 gorm 的
  `autoCreateTime/autoUpdateTime` 關閉。`total` 不落地，`Reconstitute` 重算。
- driver error 在 `infra/postgres.translate` 轉成 domain error：`gorm.ErrRecordNotFound → order.ErrNotFound`、
  `gorm.ErrDuplicatedKey → order.ErrDuplicate`，其他保持 wrapped 給 log。
- integration test（`infra/postgres`、`apps/migrate`）在 `TEST_DATABASE_URL` 空時 skip；CI 的 test job 起 Postgres service。

## Local infra（Compose）

```bash
task compose-up      # podman compose（沒有 podman 就 docker compose）起 Postgres（:5432，user/pass/db 都是 app）＋ otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up   # 自動 seed 的 configs/local.yaml 已把 postgres.url 指向它，不用 export
task run             # app 在本機跑，不進容器
task worker          # 事件 consumer process（另開一個 terminal）
task compose-down    # 連 volume 一起清
task grafana-reload  # 改了 deployments/docker/grafana/dashboards/*.json 之後
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
| HTTP routes | `http_server_request_duration_seconds`（otelgin） | `/api/*` 的 route / status、p95 |
| Go runtime | `go_goroutine_count`、`go_memory_used_bytes`… | OTLP 那套名字（`/api/metrics` 的 `go_memstats_*`、`process_*` 是 client_golang，不經 OTLP） |
| Logs | Loki `{service_name="$service"} \| severity_text=~"WARN\|ERROR"` | severity 是 structured metadata，要用 `\|` 不是 `\|=` |

踩雷：Grafana 13 的 file provisioner 用 inotify 監看資料夾，podman machine / Docker Desktop 的 bind mount 在 macOS 上
不會把事件傳進容器，所以改 JSON 不會自動重載 → `task grafana-reload`。另外 OTLP 的 metric export 間隔 60s，
`rate()` 剛啟動前兩分鐘是空的。`compose-down -v` 會清掉 Grafana 的 DB，但 provisioning 會重建 dashboard。

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
| `log.level` / `log.format` | `APP_LOG_LEVEL` / `APP_LOG_FORMAT` | `info` / `json` | |
| `postgres.url` | `APP_POSTGRES_URL` | 空 | 空 = in-memory；`apps/migrate` 必填 |
| `otel.endpoint` | `APP_OTEL_ENDPOINT` | 空 | OTLP/gRPC `host:port`；空 = telemetry 關 |
| `otel.insecure` | `APP_OTEL_INSECURE` | `false` | plaintext gRPC（本機 collector） |
| `otel.service_name` | `APP_OTEL_SERVICE_NAME` | 空 | `service.name`；空 = 執行檔名 |
| `worker.consumer_group` | `APP_WORKER_CONSUMER_GROUP` | `worker` | `apps/worker` 的 offset cursor 前綴，實際 group 是 `<值>.<bc>`（每個 BC 各自讀完整個 topic）；同名 replica 分工，新名字從頭讀 |
| — | `APP_CONFIG` | 空 | 指定 config 檔路徑（Taskfile 的 `CONFIG` var 會設它） |
| — | `TEST_DATABASE_URL` | 空 | 只給 integration test |

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto` → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`，用 `decorate(name, rec, uc)` 包起來
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
