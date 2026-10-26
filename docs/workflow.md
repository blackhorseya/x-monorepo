# Workflow：新增東西、拆 BC、rename

<sub>← [README](../README.md)</sub>

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto`，request 欄位的形狀規則（必填 / 範圍 / pattern）用 `buf.validate` 寫在 message 上，不要在 adapter 手刻
   → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`，讀取用 `decorate(name, rec, uc)`，寫入用 `decorateTx(name, rec, tx, uc)`
4. `adapters/rpc` 實作新方法（`var _ XxxServiceHandler` 會提醒）→ `task wire`

### 新增 Bounded Context（`pkgs/inventory` 是照這個清單加的第二個 BC，可當範本；`pkgs/identity` 是沒有 event 的版本：沒有 consumer / events adapter / outbox / inbox / poison，`apps/worker` 與 `NewPoisonService` 都不用加）
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
7. `task gen-ts`（需要網路）把 TS client 產進 `web/packages/proto/src/gen/<newbc>/`——CI 的 gen-freshness 也比這個

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
