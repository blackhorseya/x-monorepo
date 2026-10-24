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
task compose-up            # 本機 Postgres :5432 + Redis :6379 + grafana/otel-lgtm（Grafana :3000、OTLP :4317）
task migrate -- up         # 跑每個 BC 的 migrations；service 啟動不會自己跑
task release-snapshot      # goreleaser dry run → bin/dist（真的 release 由 main 上的 CI 綠燈自動觸發，不手動打 tag；沒改到會進 artifact 的檔案就被 gate 跳過，task release-gate 先看：列 releasable 檔 + 預測版號）
task poison -- list        # 看各 BC 的 poison 表；requeue <bc> <offset>…|all 送回原 topic、drop 刪掉（worker poison 子命令）
task prune -- --older-than 720h --dry-run   # 刪每個 group 都 ack 過的 outbox 列 + 消費過的 inbox 列（worker prune；先 dry-run；NOTE 列出擋住的 group，--forget-group 刪掉不會再跑的）
task helm-check            # deployments/helm/x-monorepo：每個 ci/*.yaml profile 跑 helm lint + kubeconform + check.py（derived 數字：grace period、hook 順序、prune args、console 的 RPC_UPSTREAM）
task k6-smoke              # tests/k6：orders（load thresholds）/ ratelimit（429 + Retry-After）/ timeout（1ns → 全部 DEADLINE_EXCEEDED）/ reservation（rpc + worker，下單 → RESERVED 延遲）各自起一個 bin/rpc 打；task k6 -- <script> 打現成 server
task helm-kind             # 真裝：kind cluster + deployments/helm/kind（拋棄式 Postgres / Redis）+ tree build 的 image（含 console：web-build → web/apps/console/Dockerfile），install → smoke → upgrade → smoke；helm-kind-down 刪 cluster
task helm-kind-keda        # 接著在同一個 cluster：KEDA + OTLP collector + Prometheus，k6 灌訂單看 worker 真的靠 eventbus_consumer_lag scale out / in
task run                   # build 後以 configs/local.yaml 跑 ./bin/rpc（Connect + gRPC + gRPC-Web 同 port；RPC 在 /api/<svc>/<Method> 與原生路徑都通，/api/docs 開）
```

跑單一測試：`go.work` 根目錄不是 module，`./...` 在那裡比不到任何東西，必須 `cd` 進 module：

```bash
cd pkgs/ordering && go test -race -run 'TestCreateGetCancel' ./application/usecase/
cd pkgs/ordering && go test -count=1 -run '^TestArchitecture$' .      # 只跑 archtest
go test -race ./infra/postgres/                                       # integration test：testcontainers 自起 Postgres（需 Docker/Podman）
TEST_DATABASE_URL=postgres://app:app@localhost:5432/app?sslmode=disable \
  go test -race ./infra/postgres/                                     # 或指定現成 DB；設了連不上是 FAIL 不是 skip
```

前端：`web/` 是 Turborepo（pnpm）。`packages/proto` 是 connect-es client（`task gen-ts` 需要網路，產出到那裡）；
`apps/console` 是 Vite + TanStack Router / Query + Tailwind v4 + shadcn/ui，`task web-dev`（proxy 到 :8080）/ `task web-typecheck` / `task web-build` /
`task web-e2e`（Playwright：`web/apps/console/e2e`，起 `bin/rpc` + vite preview 真打；`E2E_DATABASE_URL` 有設就先 `migrate up` 走 Postgres 並由 `e2e/worker.setup.ts` 多起一個 `bin/worker`（CI；`reservation.spec.ts` 驗 outbox → worker → RESERVED / RELEASED，`reservations.spec.ts` 驗 /reservations 的 status filter 與 `?order=` pin），沒設是 in-memory，spec 用 `e2e/mode.ts` 的 `withDB` 分支；project `auth`（`e2e/auth/`）另起一個 `auth.mode=required` 的 rpc + preview 驗 credential 流程（API key 與 `jwt.ts` 現場簽的 HS256 JWT；`helpers.ts` / `jwt.ts` 不是 spec，project 的 testMatch 只收 `*.spec.ts`）；locator 用 `getByLabel` / `getByRole`，第一次先 `pnpm exec playwright install chromium`）。
shadcn component 用 `pnpm dlx shadcn@latest add <name>` 加，產出的 `cn` import 要確認是 `@/lib/utils`。

**新 clone / 新 worktree 第一件事**：`task tools`。`./bin` 是 gitignored，而 `task gen`（也被 `gate-local` 間接呼叫）會**先刪掉**
所有 `*.pb.go` / `*.connect.go` 再呼叫 `bin/protoc-gen-*`；工具不在時會留下一個生成檔全被刪掉的 tree。
遇到了就 `git checkout -- pkgs` 還原，**不要**用 `git commit -a` 把刪除一起 commit 進去。
生成物（pb.go、connect.go、`wire_gen.go`、`contract/v1/openapi.yaml`）全部有 commit，gen-freshness 靠 `git diff` 比對它們。

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
pkgs/platform    技術共用：contextx、config、server（gin + Connect）、usecasex、otel、postgres、archtest；
                 admin（admin.v1.AdminService：poison 操作 + Prune RPC，與 worker 的 CLI 同一組 eventbus 函式；contract 在 protos/admin，不屬於任何 BC）
pkgs/<bc>        一個 bounded context = 完整六角形（示範 BC：ordering；inventory 是第二個，消費 ordering 的 event）
  provider.go      wire.ProviderSet，對外唯一出口；usecase 在這裡被 decorate() / decorateTx() 包起來
  contract/v1/     buf 生成的 wire format（pb + Connect + openapi.yaml），只有 adapters / apps 可 import
  domain/<agg>/    aggregate、VO、Repository interface、domain errors（手寫，不是 proto）
  application/     dto（手寫 application 邊界）、usecase（orchestration）、port、mapper
  infra/           driven：memory / postgres（gorm row model + embed migrations）/ system（clock、id）
  adapters/        driving：rpc（Connect handler，pb ↔ dto）、consumer（broker handler）
apps/rpc         composition root：main + wire.go，無業務、無 mapping
apps/worker      事件 consumer：每個 BC 的 Subscriptions 掛同一個 Watermill router，各 BC 自己的 consumer group / poison；
                 worker.broker=redis 時自己當 forwarder 把 outbox 表轉進 Redis Streams 再讀（outbox 仍是寫入路徑）；
                 `worker poison list|show|requeue|drop` 操作各 BC 的 poison 表（requeue = 重新 insert 回原 topic 的 outbox 表）；
                 `worker prune --older-than <d>` 是 outbox / inbox 的 retention（poison 表不清）；
                 有 DB 時 `eventbus.Lag` 另報每張 outbox 表 / inbox 的大小與最老一列年齡（`eventbus_outbox_*` / `eventbus_inbox_*`），alert 用它抓 prune 沒跑與 stale group
apps/migrate     收集各 BC 的 Migration()，唯一能 import infra 的 app
deployments/helm Helm chart：rpc / worker Deployment、migrate pre-install/pre-upgrade hook Job、prune CronJob、console（nginx，/api proxy 到 rpc Service，`console.enabled` 預設 off）；terminationGracePeriodSeconds
                 從 rpc.shutdown / worker 的 drain 算出來（改 shutdown 行為要一起改 values 註解與 check.py）；CI 的 helm-kind job 在 kind 真裝 + smoke
```

**depguard（`.golangci.yml`）強制的 import 方向**：

| 套件 | 可以 import | 不可以 |
|---|---|---|
| domain | kernel | 其他全部 |
| application | domain、kernel、`platform/{contextx,usecasex}` | infra / adapters / contract |
| infra | domain、application/port、platform | adapters / contract |
| adapters | application、contract、platform、其他 BC 的 `contract/` | 其他 BC 的 domain / application / infra / adapters |
| kernel、platform | 標準庫、第三方 | 任何 BC |
| apps/* | 各 BC 的 `provider.go`、platform | usecase（只透過 wire）、infra |
| 任何 BC 的 domain / application / infra | — | 其他 BC 的任何東西（走 port 或 event） |

**archtest（`pkgs/platform/archtest`，每個 BC 的 `arch_test.go`）強制的 DDD 形狀**：domain struct 欄位 private、
無 `Set*`、每個 struct 有 constructor（`*Params` 除外）、不呼叫 `time.Now()`、不 log；`*Repository` interface
只能在 domain；usecase 只有 `Execute(c, in) (out, error)` 一個公開方法；dto 無方法、不 import domain / proto；
port 只有 interface；adapter 持有 `usecasex.UseCase[I,O]`，不持有 `*usecase.X`。

幾個容易踩的設計點：

- **contract ≠ dto**：contract 是生成的 wire format，dto 是手寫的 application 邊界，只放 usecase 需要的欄位；
  同一個 usecase 被 rpc 和 consumer 共用時 dto 是共同語言。不要照 pb 抄 dto。
- **decorator 只在 `provider.go` 加**：`decorate()` = metrics + logging，給讀取；`decorateTx()` 再多 `usecasex.Transactional` + 最內層的 `usecasex.Idempotent`（consumer inbox：
  `<bc>.inbox` 表記 message id，重送在 usecase 之前以 `message.duplicate` drop；rpc path 沒 message id、沒 DB 時 inbox 是 nil，都是 pass-through），給寫入。
  rpc 與 consumer 自動都有。**usecase 不自己開 transaction**，也不持有 `port.Transactor`；直接用收到的 `c` 呼叫 repo / publisher，
  它們從 context 拿到 tx。
- **Domain error** 用 `kernel.NewError(kind, code, msg)`；`server.ToConnectError` 依 `Kind` 映射 Connect code，
  `Code` 放 `x-error-code` header。consumer 把 domain error 視為 drop（不重試），infra error 才 retry——所以「業務上的失敗結果」
  （例如 inventory 缺貨）要做成 aggregate 狀態 + event，不能做成 error；反過來 optimistic-lock 的 `stock.ErrStale` / `order.ErrStale` 刻意不是 `kernel.Error`：
  它們 wrap `kernel.ErrConcurrentUpdate`，consumer 會 retry，rpc 回 `Aborted`（`x-error-code: concurrent_update`），metrics 記 `kind=aborted`。
- **Optimistic lock**：aggregate 帶 `version`，`Save` 比對後 +1。存完後記憶體中的 aggregate 版本**不會**前進，要再存一次請先 reload。
- **Validation**：request 形狀由 protovalidate interceptor 擋（`x-error-code: request.validation_failed` + `Violations` detail），
  consumer path 沒有它，所以 domain 的檢查不能拿掉。
- **Auth 只做 authentication**：`auth.mode` 預設 `off`（`task run`、E2E、console 都沒帶 credential）；開了之後 `server.NewAuthInterceptor`
  把 `X-API-Key` / `Authorization: Bearer <jwt>` 解析成 `contextx.Principal` 放進 context，usecase 用 `contextx.PrincipalFrom(c)` 讀，
  consumer path 永遠是 anonymous。`rpc` access line 的 `auth` / `sub` 與 span 的 `enduser.id` 由 auth interceptor 透過 logging interceptor 留在 context 的 slot 回報（`recordAuth`），不要把 principal 塞進 metrics label。要不要做授權（role 檢查）是讀的人的事，不要在 interceptor 裡加。
  Interceptor chain 由外而內：`tracing, meta, logging, [rate limit], [auth], [timeout], validate`——rate limit 在 auth 之外（洪水先擋再驗簽章）、
  timeout 在 auth 之內（deadline 算 validate + usecase）；`http.timeout` 到期的失敗一律 `DEADLINE_EXCEEDED` + `request.timeout`，超過 `http.rate_limit` 是 `RESOURCE_EXHAUSTED` + `request.rate_limited` + `Retry-After`。
  所有 interceptor 都是 unary-only：哪天加了 streaming RPC，auth 要另外用 `WrapStreamingHandler` 包，不然 stream 沒有驗。
- **Graceful shutdown 是有測試保護的行為**：rpc 收到 SIGTERM 先把 `/api/readyz` 翻 503 再服務 `http.shutdown_delay`（預設 0），然後關 listener、
  in-flight request 有 `http.shutdown_timeout`；worker 的 `eventbus.Register` 把 subscriber 包進 `eventbus.Draining`，正在跑的 handler 跑完 + ack 才結束
  （上限 8s）。每次 handler attempt 另有 `worker.handler_timeout`（預設 10s，0 = 不限）：到期的 attempt 照常 retry，用完進 poison。改 `Server.Serve` / `Draining` 請一起跑 `platform/server` 與 `platform/eventbus` 的 shutdown tests。
- **Response meta 走 header**（`X-Request-Id`、`X-Trace-Id`、`traceparent`），不包進 proto message。
- **Persistence**：每個 BC 一個 Postgres schema（`ordering.orders`、`ordering.schema_migrations`）；SQL 放
  `pkgs/<bc>/infra/postgres/migrations/NNNN_name.{up,down}.sql` 由 BC embed（go.work 下 embed 不能跨 module）。
  `postgres.url` 空 → `*gorm.DB` 為 nil → `ProvideOrderRepository` 改用 in-memory，所以 `task run` 與 E2E test 不需要 DB。
- **`wire.go` 是 AppConfig → component config 的唯一 mapping 點**。

## Config

`platform/config.Load()`：defaults → YAML（`$APP_CONFIG`，否則 `./configs/config.yaml`）→ `APP_*` env，由低到高。
`configs/local.yaml` gitignored，`task run` / `task migrate` 第一次會從 `config.example.yaml` 複製。
注意：viper 忽略空 env，`APP_POSTGRES_URL=` 清不掉 key，要「不接 DB」請改 local.yaml。完整 key 表見 README。
`auth.*`（mode / api_keys / jwt / public）見 README 的 Config 表。

## 新增東西的步驟

**新增 RPC**：改 `protos/<bc>/v1/service.proto`（request 欄位規則用 `buf.validate` 寫在 proto，interceptor 會擋；domain 仍保留不變量）→ `task gen`（`task breaking` 檢查相容）→ domain 加行為 →
`application/dto` + `usecase` → `provider.go` 加 `ProvideXxx` 並用 `decorate()`（讀）或 `decorateTx()`（寫）包 → `adapters/rpc` 實作
（`var _ XxxServiceHandler` 會提醒）→ `task wire`。

**新增 Bounded Context**（照 `pkgs/inventory` 抄）：`protos/<newbc>/v1/{entity,service,event}.proto` + `openapi.base.yaml` 與
`buf.gen.yaml` 的 `go_package` override → `pkgs/<newbc>/go.mod`（replace 指向 kernel / platform / 要讀 contract 的 BC）+ `go.work`
加 `use` + `Taskfile.yml` 的 `MODULES` 與 `BCS` + `apps/*/go.mod` 的 require / replace → `task tidy` → `contract/v1/openapi.go` →
`task gen` → 複製 `pkgs/ordering` 結構寫 `provider.go`（`Subscriptions` 自己的 named type）→ `.golangci.yml` 加該 BC 的 layer block 與
`bc-isolation-<bc>`，並把它加進其他 BC / shared / apps 的 deny（`pkg` 是 prefix match，不能用 `*`）→ `apps/rpc` 的 `NewRoutes` /
`NewDocs` / `NewPoisonService` 的 schema 清單加它、`apps/{rpc,worker}/wire.go` 加 `ProviderSet`、`apps/worker` 的 `NewWorker` 收它的 `Subscriptions`、`poison.go` 的 `schemas` 加它 → `task wire` →
有 DB 則加 migrations（含自己 topic 與 poison 的 events 表、`<bc>.inbox` 表）與 `apps/migrate/main.go` 的 `migrations`；`application/port` 加自己的 `Inbox` 與 `provider.go` 的 `ProvideInbox`（每個 BC 自己的型別，wire 才分得開）。

## Conventions（本 repo 特有）

- goimports local-prefix：`github.com/blackhorseya/x-monorepo`
- 所有 binary / test binary / coverage 輸出到 `./bin/`
- Receiver：domain `x`、usecase `uc`、infra `i`、handler `h`；interface 參數 `c context.Context`，
  實作內 `ctx := contextx.FromContext(c)`
- PR title 遵循 Conventional Commits
