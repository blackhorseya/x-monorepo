# x-monorepo

Protobuf-first Go monorepo：**Clean Architecture + DDD（BC-first）**，go.work 多 module，
Connect 對外，Wire 組裝，Turborepo 前端。

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
task run            # ./bin/rpc，Connect + gRPC + gRPC-Web 同一個 port；APP_HTTP_DOCS=true 開 /docs

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
│   ├── platform/                  # 技術共用：contextx、config、server、usecasex、contract/common
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
│       └── adapters/              # driving：transport
│           ├── rpc/               # Connect handler；pb ↔ dto
│           └── consumer/          # broker handler；呼叫同一個 CancelOrder usecase
├── apps/
│   └── rpc/                       # composition root：main + wire，無業務、無 mapping
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
| `apps/*` | 各 BC 的 `provider.go`、platform | 直接呼叫 usecase（只透過 wire） |
| 任何 BC | — | 其他 BC（走 port 或 event） |

**contract 和 dto 的差別**：contract 是 wire format（有版本、跨語言、生成），dto 是 application 邊界（手寫、
只放 usecase 需要的欄位）。同一個 usecase 被 rpc 和 consumer 兩個 adapter 呼叫時，dto 就是它們的共同語言。
dto 不要照著 pb 抄。

## usecasex：use case 的統一形狀與 decorator

```go
type UseCase[I, O any] interface { Execute(c context.Context, in I) (O, error) }
```

adapter 只依賴 `UseCase[I, O]`，不依賴具體 struct。`provider.go` 用 `usecasex.Chain` 把 logging（之後加
metrics、tx）包在 usecase 外面，rpc 和 consumer 兩邊自動都有。Connect interceptor 只管 transport 層的事
（procedure、protocol、code）。

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build；`buf breaking` **只警告** | 全 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環

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
時才掛 `/docs`（Scalar UI）與 `/openapi/<bc>.yaml`。production 不開。前端不需要它 —— `web/packages/proto`
直接從 proto 生 connect-es client。

## Conventions

- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`
- Interface 參數 `c context.Context`；實作內 `ctx := contextx.FromContext(c)`
- Domain error 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 code，`Code` 放
  `x-error-code` header；consumer 把 domain error 視為 drop（不重試），infra error 才 retry
- `wire.go` 是 AppConfig → component config 的唯一 mapping 點
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- goimports local-prefix `github.com/blackhorseya/x-monorepo`

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto` → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`（含 decorator）
4. `adapters/rpc` 實作新方法（`var _ XxxServiceHandler` 會提醒）→ `task wire`

### 新增 Bounded Context
1. `protos/<newbc>/v1/{entity,service}.proto`；`buf.gen.yaml` 加一行 `go_package` override
2. `pkgs/<newbc>/go.mod`（module path 對齊目錄），`go.work` 加 `use`，`Taskfile.yml` 的 `MODULES` 加上
3. 複製 `pkgs/ordering` 結構，寫 `provider.go`
4. `.golangci.yml` 加該 BC 的 depguard block
5. `apps/rpc`：`NewRoutes` 加參數、`wire.go` 加 `ProviderSet` → `task wire`

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
