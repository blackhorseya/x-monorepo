# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Protobuf-first Go monorepo：Clean Architecture + DDD（bounded context 優先）、`go.work` 多 module、
Connect 對外、Wire 組裝、Turborepo 前端（`web/`）。細節以 `README.md` 為準，這份只放「開工就要知道」的事。

## 常用指令

所有檢查的單一事實來源是 `Taskfile.yml`；hooks、lefthook、CI 都只是呼叫它。

```bash
task tools                 # 固定版本工具鏈（buf / wire / golangci-lint / protoc-gen-*）→ ./bin，第一次必跑
task tidy                  # 每個 module go mod tidy + go work sync
task gen wire              # protos → pkgs/<bc>/contract（pb + Connect + openapi.yaml）；DI → apps/*/wire_gen.go
task lint                  # buf lint + go vet + golangci-lint（depguard 強制分層 / BC 邊界）
task test                  # go test -race，每個 module
task gate-fast FILE=a.go   # gofmt + vet + golangci-lint --fast-only + archtest，只跑該檔所屬 module（秒級）
task gate-local            # gen freshness + buf lint + affected modules 的 go test -race（~1 min）
task gate-ci               # 與 CI 等價的全量檢查
task breaking              # proto 對 main 做 buf breaking
task compose-up            # 本機 Postgres :5432 + grafana/otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up         # 跑每個 BC 的 migrations；service 啟動不會自己跑
task run                   # build 後以 configs/local.yaml 跑 ./bin/rpc（Connect + gRPC + gRPC-Web 同 port，/api/docs 開）
```

跑單一測試：`go.work` 根目錄不是 module，`./...` 在那裡比不到任何東西，必須 `cd` 進 module：

```bash
cd pkgs/ordering && go test -race -run 'TestCreateOrder' ./application/usecase/
cd pkgs/ordering && go test -count=1 -run '^TestArchitecture$' .      # 只跑 archtest
TEST_DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable \
  go test -race ./infra/postgres/                                     # integration test，env 空時 skip
```

前端：`cd web && pnpm install && pnpm build`（Turborepo；`task gen-ts` 需要網路，產出到 `web/packages/proto`）。

## Hook harness（會擋你的東西）

`.claude/settings.json` 掛了兩個 hook，exit 2 時 stderr 會回到 agent：

- **PostToolUse（Edit / Write）**：對 `.go / .proto / go.mod / go.work / buf*.yaml` 跑 `task gate-fast`。
  用 Bash（sed、heredoc）改檔**不會**觸發，改完請手動 `task gate-fast FILE=<path>`。
- **Stop**：跑 `task gate-local`，失敗就不讓你宣告完成。gen-freshness 是和 HEAD 比 diff，
  改了 proto 或 wire provider 後要 `task gen wire` 並把產物一起 commit。

CI（`.github/workflows/ci.yml`）另外有 `work-sync`（go.mod 不得漂移，加依賴後 `task tidy`）、
`cover-gate`（所有 `*/domain/*` package 必須 100% 覆蓋）、`buf breaking`（只警告）。

## 架構：誰依賴誰

```
pkgs/kernel      shared kernel：Money VO、kernel.Error{Kind, Code}。零依賴
pkgs/platform    技術共用：contextx、config、server（gin + Connect）、usecasex、otel、postgres、archtest
pkgs/<bc>        一個 bounded context = 完整六角形（示範 BC：ordering）
  provider.go      wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorate() 包起來
  contract/v1/     buf 生成的 wire format（pb + Connect + openapi.yaml），只有 adapters / apps 可 import
  domain/<agg>/    aggregate、VO、Repository interface、domain errors（手寫，不是 proto）
  application/     dto（手寫 application 邊界）、usecase（orchestration）、port、mapper
  infra/           driven：memory / postgres（gorm row model + embed migrations）/ system（clock、id）
  adapters/        driving：rpc（Connect handler，pb ↔ dto）、consumer（broker handler）
apps/rpc         composition root：main + wire.go，無業務、無 mapping
apps/migrate     收集各 BC 的 Migration()，唯一能 import infra 的 app
```

**depguard（`.golangci.yml`）強制的 import 方向**：

| 套件 | 可以 import | 不可以 |
|---|---|---|
| domain | kernel | 其他全部 |
| application | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| infra | domain、application/port、platform | adapters / contract |
| adapters | application、contract、platform | 其他 BC |
| kernel、platform | 標準庫、第三方 | 任何 BC |
| apps/* | 各 BC 的 `provider.go`、platform | usecase（只透過 wire）、infra |
| 任何 BC | — | 其他 BC（走 port 或 event） |

**archtest（`pkgs/platform/archtest`，每個 BC 的 `arch_test.go`）強制的 DDD 形狀**：domain struct 欄位 private、
無 `Set*`、每個 struct 有 constructor（`*Params` 除外）、不呼叫 `time.Now()`、不 log；`*Repository` interface
只能在 domain；usecase 只有 `Execute(c, in) (out, error)` 一個公開方法；dto 無方法、不 import domain / proto；
port 只有 interface；adapter 持有 `usecasex.UseCase[I,O]`，不持有 `*usecase.X`。

幾個容易踩的設計點：

- **contract ≠ dto**：contract 是生成的 wire format，dto 是手寫的 application 邊界，只放 usecase 需要的欄位；
  同一個 usecase 被 rpc 和 consumer 共用時 dto 是共同語言。不要照 pb 抄 dto。
- **decorator 只在 `provider.go` 的 `decorate()` 加**（metrics + logging，之後加 tx 也在這裡），rpc 與 consumer 自動都有。
- **Domain error** 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 Connect code，
  `Code` 放 `x-error-code` header。consumer 把 domain error 視為 drop（不重試），infra error 才 retry。
- **Response meta 走 header**（`X-Request-Id`、`X-Trace-Id`、`traceparent`），不包進 proto message。
- **Persistence**：每個 BC 一個 Postgres schema（`ordering.orders`、`ordering.schema_migrations`）；SQL 放
  `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql` 由 BC embed（go.work 下 embed 不能跨 module）。
  `postgres.url` 空 → `*gorm.DB` 為 nil → `ProvideOrderRepository` 改用 in-memory，所以 `task run` 與 E2E test 不需要 DB。
- **`wire.go` 是 AppConfig → component config 的唯一 mapping 點**。

## Config

`platform/config.Load()`：defaults → YAML（`$APP_CONFIG`，否則 `./configs/config.yaml`）→ `APP_*` env，由低到高。
`configs/local.yaml` gitignored，`task run` / `task migrate` 第一次會從 `config.example.yaml` 複製。
注意：viper 忽略空 env，`APP_POSTGRES_URL=` 清不掉 key，要「不接 DB」請改 local.yaml。完整 key 表見 README。

## 新增東西的步驟

**新增 RPC**：改 `protos/<bc>/v1/service.proto` → `task gen`（`task breaking` 檢查相容）→ domain 加行為 →
`application/dto` + `usecase` → `provider.go` 加 `ProvideXxx` 並用 `decorate()` 包 → `adapters/rpc` 實作
（`var _ XxxServiceHandler` 會提醒）→ `task wire`。

**新增 Bounded Context**：`protos/<newbc>/v1/{entity,service}.proto` 與 `buf.gen.yaml` 的 `go_package` override →
`pkgs/<newbc>/go.mod` + `go.work` 加 `use` + `Taskfile.yml` 的 `MODULES`（有 OpenAPI 則 `BCS`）→ 複製 `pkgs/ordering`
結構寫 `provider.go` → `.golangci.yml` 加該 BC 的 depguard block（`pkg` 是 prefix match，不能用 `*`）→
`apps/rpc` 的 `NewRoutes` 加參數、`wire.go` 加 `ProviderSet` → `task wire` → 有 DB 則加 migrations 與
`apps/migrate/main.go` 的 `migrations`。

## Conventions（本 repo 特有）

- goimports local-prefix：`github.com/blackhorseya/x-monorepo`
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`；interface 參數 `c context.Context`，
  實作內 `ctx := contextx.FromContext(c)`
- PR title 遵循 Conventional Commits
