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
task run            # ./bin/rpc，Connect + gRPC + gRPC-Web 同一個 port；dev 預設開 /api/docs（production 要 APP_HTTP_DOCS=true 才有）
                    # 沒設 postgres.url 就用 in-memory repository；要接 Postgres：
task compose-up      # 本機 Postgres；再 cp configs/config.example.yaml configs/config.yaml 填 postgres.url（或 export APP_POSTGRES_URL=…）
task migrate -- up  # 跑每個 BC 的 migrations（service 啟動時不會自己跑）
task run

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
│   └── ordering/v1/
│       ├── entity.proto           # aggregate 的 state schema（跨語言契約，不是 Go domain model）
│       ├── service.proto          # RPC
│       └── event.proto            # 進出 broker 的訊息
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
│           └── consumer/          # broker handler；呼叫同一個 CancelOrder usecase
├── deployments/docker/            # docker-compose.yaml：本機 infra（Postgres），app 不進容器
├── apps/
│   ├── rpc/                       # composition root：main + wire，無業務、無 mapping
│   └── migrate/                   # 收集各 BC 的 embed migrations，up / down / version；唯一能 import infra 的 app
└── web/                           # Turborepo：apps/*、packages/proto（@repo/proto）
```

## Dependency rules（`.golangci.yml` depguard 強制）

| 套件 | 可以 import | 不可以 import |
|---|---|---|
| `domain` | `kernel` | application / infra / adapters / contract / platform |
| `application` | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| `infra` | domain、application/port、platform | adapters / contract |
| `adapters` | application、contract、platform | 其他 BC |
| `kernel`、`platform` | 標準庫、第三方 | 任何 BC |
| `apps/*` | 各 BC 的 `provider.go`、platform | 直接呼叫 usecase（只透過 wire）、infra |
| `apps/migrate` | 各 BC 的 `infra/postgres`（只為了拿 `Migration()`）、platform | domain / application / adapters |
| 任何 BC | — | 其他 BC（走 port 或 event） |

**contract 和 dto 的差別**：contract 是 wire format（有版本、跨語言、生成），dto 是 application 邊界（手寫、
只放 usecase 需要的欄位）。同一個 usecase 被 rpc 和 consumer 兩個 adapter 呼叫時，dto 就是它們的共同語言。
dto 不要照著 pb 抄。

## server：gin + Connect

`platform/server` 用 gin 當 HTTP router：每個 BC 的 Connect handler 以 `gin.WrapH` 掛在 `/<pkg>.<Service>/*procedure`，
`/healthz`、`/metrics`、`/api/docs` 是普通 gin route。`RedirectTrailingSlash` / `RedirectFixedPath` 關閉（gRPC client
不跟 redirect）；gin 只做 routing + `Recovery()`，RPC log 留在 Connect interceptor。HTTP/1.1 與 h2c 同一個 port，
Connect / gRPC / gRPC-Web 三種協定都有 E2E test（`apps/rpc/main_test.go`）。

## usecasex：use case 的統一形狀與 decorator

```go
type UseCase[I, O any] interface { Execute(c context.Context, in I) (O, error) }
```

adapter 只依賴 `UseCase[I, O]`，不依賴具體 struct。`provider.go` 的 `decorate()` 用 `usecasex.Chain` 把
metrics + logging 包在每個 usecase 外面（之後加 tx 也在這一處），rpc 和 consumer 兩邊自動都有。Connect
interceptor 只管 transport 層的事（procedure、protocol、code）。

- `usecasex.Logging`：一行 log，domain error 是 `Info`（預期結果），其他 error 才是 `Error`
- `usecasex.Metrics`：histogram `usecase_duration_seconds{usecase, kind}`；`kind` = `ok` / kernel error kind /
  `internal`，和 `server.ToConnectError` 同一套判準，label 值域有限。registry 由 `platform/metrics.ProviderSet`
  注入（每個 `Initialize()` 一個，不用 global），`server` 掛在 `GET /metrics`

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
| BC 互不 import；kernel / platform 不 import BC；apps 只碰 provider.go | 邊界 | **depguard** |
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
  `ordering.ProvideOrderRepository` 看到 nil 就改用 `infra/memory`，所以 `task run`、E2E test 不需要 DB。
- **每個 BC 一個 Postgres schema**（`ordering.orders`），版本表也在該 schema 裡（`ordering.schema_migrations`）。
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
task compose-up      # podman compose（沒有 podman 就 docker compose）起 Postgres（:5432，user/pass/db 都是 app）
export APP_POSTGRES_URL='postgres://app:app@127.0.0.1:5432/app?sslmode=disable'   # 或寫進 configs/config.yaml
task migrate -- up
task run             # app 在本機跑，不進容器
task compose-down    # 連 volume 一起清
```

## Config（viper）

`platform/config.Load()` 的來源由低到高：defaults → YAML 檔 → `APP_*` 環境變數。YAML 找 `$APP_CONFIG`（指定就必須存在），
否則依序找 `./configs/config.yaml`、`./config.yaml`，都沒有也沒關係。每個 key 都有 default，所以只靠環境變數就能跑。
範本在 `configs/config.example.yaml`；`configs/config.yaml` 已 gitignore。

| YAML key | 環境變數 | 預設 | 說明 |
|---|---|---|---|
| `http.addr` | `APP_HTTP_ADDR` | `:8080` | |
| `http.docs` | `APP_HTTP_DOCS` | `false` | 掛 `/api/docs`、`/api/openapi/<bc>.yaml`（`task run` 會開） |
| `log.level` / `log.format` | `APP_LOG_LEVEL` / `APP_LOG_FORMAT` | `info` / `json` | |
| `postgres.url` | `APP_POSTGRES_URL` | 空 | 空 = in-memory；`apps/migrate` 必填 |
| — | `APP_CONFIG` | 空 | 指定 config 檔路徑 |
| — | `TEST_DATABASE_URL` | 空 | 只給 integration test |

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto` → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`，用 `decorate(name, rec, uc)` 包起來
4. `adapters/rpc` 實作新方法（`var _ XxxServiceHandler` 會提醒）→ `task wire`

### 新增 Bounded Context
1. `protos/<newbc>/v1/{entity,service}.proto`；`buf.gen.yaml` 加一行 `go_package` override
2. `pkgs/<newbc>/go.mod`（module path 對齊目錄），`go.work` 加 `use`，`Taskfile.yml` 的 `MODULES` 加上
3. 複製 `pkgs/ordering` 結構，寫 `provider.go`
4. `.golangci.yml` 加該 BC 的 depguard block
5. `apps/rpc`：`NewRoutes` 加參數、`wire.go` 加 `ProviderSet` → `task wire`
6. 有 DB 的話：`infra/postgres/migrations/0001_*.sql`（表都放在 `<newbc>.` schema）+ `Migration()`，
   `apps/migrate/main.go` 的 `migrations` 加一筆

### 新增 consumer process
`apps/consumer/`：複製 `apps/rpc` 的 `wire.go`，不掛 `server.New`，改成訂閱 broker 並把
`consumer.OrderCancellationHandler.Handle` 接上；error contract 見 `adapters/consumer/handler.go`。
要在同一個 process 跑 rpc + consumer 也可以（go-micro 式），那是 `apps/` 的決定，BC 不在乎。

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
