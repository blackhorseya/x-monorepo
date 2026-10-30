# Workflow：新增東西、拆 BC、rename

<sub>← [README](../README.md)</sub>

## Workflow

### 新增 RPC
1. 改 `protos/<bc>/v1/service.proto`，request 欄位的形狀規則（必填 / 範圍 / pattern）用 `buf.validate` 寫在 message 上，不要在 adapter 手刻
   → `task gen`（`task breaking` 檢查相容）
2. domain 加行為 → `application/dto` + `usecase`
3. `provider.go` 加 `ProvideXxx`，讀取用 `decorate(name, rec, uc)`，寫入用 `decorateTx(name, rec, tx, uc)`
4. `adapters/rpc` 實作新方法（`var _ XxxServiceHandler` 會提醒）→ `task wire`

### 新增 Bounded Context（`pkgs/inventory` 是照這個清單加的第二個 BC，可當範本；`pkgs/identity` 與 `pkgs/catalog` 是沒有 event 的版本：沒有 consumer / events adapter / outbox / inbox / poison，`apps/worker` 與 `NewPoisonService` 都不用加）
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

## Review contracts

review 反覆抓到、但現有 CI 擋不住的類別。PR template（`.github/pull_request_template.md`）把它們列成 checkbox；實作者的派工 brief、PR 驗證模板與 reviewer 的 brief 都照這份對（brief 要明示適用的小節）。不適用就在 PR 寫 `N/A` 與一句理由。

### proto 變更：本機驗證要明列 TS freshness（#229）
- 驗證回報逐項列 Go、OpenAPI、Wire、TS 的指令與結果，不能只寫「gen 乾淨」。
- `task check-gen-fresh` 只涵蓋 Go / OpenAPI / Wire，**不是** TS 的證據；TS 要用 `task check-gen-fresh-all`（= CI gen-freshness（CI 直接呼叫這個 task，只有一份定義）：`task gen wire gen-ts` 後不帶 pathspec 的 `git diff`，範圍與 CI 相同；先 `git add` 你的改動，否則未 stage 的修改也會被算成 diff）。
- `gen-ts` 需要網路：連不上就回報「TS 未驗證」，不要記成已新鮮。CI 的 gen-freshness 仍是 fail-closed gate。

### 單一 BC 的 rollback runbook：驗其他 BC 不動（#319）
- 新增或修改 BC 的 rollback runbook（`docs/operations.md`），PR 要附 `apps/migrate/rollback_runbook_test.go` 的 `rollbackCase`：測試從文件取出 runbook 實際給的入口（marker 後第一個 ```sql 區塊，或它指向的指令）在隔離 Postgres 執行，斷言目標 BC 只退一版、其他 BC 的版本、表、欄位與資料不變。不可用 `postgres.Down(<某 BC>)` 代替文件入口。
- runbook 聲稱適用的每個狀態（clean、dirty……）各一個 `rollbackCase`；文件沒寫、也寫不出測試的狀態就不要聲稱。全域 `task migrate -- down` 讓每個 BC 各退一步，語意不改；runbook 不得用它當單一 BC 的 rollback。

### migration 宣稱線上可用：驗完整交易的鎖（#228）
- 文件或 migration 註解若宣稱遷移期間讀／寫可用，必須用**完整 migration 交易**驗證，不能由單一 SQL 的 lock 級別推論（例：`CREATE INDEX` 只持 SHARE，但同交易前面的 `ALTER TABLE` 會持 AccessExclusive 到 commit）。
- 做法（拋棄式 DB）：session A 執行完整 migration、先不 commit；session B 查 `pg_locks`，並以短 `lock_timeout` 執行代表性 SELECT／寫入；記錄結果，部署文件與結果一致；最後 rollback／commit 並關閉兩個 session。
- 只用在含此類宣稱的 migration。離線 migration 持 exclusive lock 是合理選擇，重點是文件如實描述停機範圍；不把特定 lock 級別設成全域禁止。

### 認證 generation／取消類測試：列出所有副作用界線，cookie 與 server session 都要驗（#225）
- 設計／review 先列「副作用與取消界線」表：JS state、Web Storage、HttpOnly cookie、server session／rotation 各自的「忽略結果」是否涵蓋瀏覽器已套用的 Set-Cookie 與後端已 commit 的寫入。generation 只是 JS 狀態界線，不會取消 cookie jar 或已提交的後端寫入；派工 brief 也要明示這點，避免把局部防護寫成全局保證。
- 凡 client 端 generation／attempt 丟棄 response body 的流程（Login、Logout、取消、重試），測試必須驅動**真 Console + 真 Login／Logout／Refresh entry point**（Playwright browser integration，不是 mocked fetch／複製 helper 的 fixture），控制 response 順序：Logout／Clear 後才釋放舊 Login／Refresh，接著 **reload**，同時斷言 cookie 與 server session（Refresh 是否成功、`revoked`）。
- **正例必保留**：未取消的正常 Login → reload → Refresh 成功。成功取消的控制組（取消後 Refresh 必須失敗）擋不住「把所有 Refresh 都拒絕」，只有正例能保護正常登入持續有效；兩者缺一不可。
- 驗收矩陣至少含：取消先到／Login 先 commit 的交錯（barrier 控制，真 handler + 真 repo）、取消後 expiry／prune 再遇原 attempt 延遲 admission、commit 後 prune／KEK 輪替再 Logout、不同 pod keyset、Logout 被 429／5xx／斷線／commit 後丟回應、多頁共享 pendingCancel 的 read／merge／write／ack 原子性。
- generation-only 單元測試可保留，但不能當完整撤銷／登出保證。fixture 只用自造 key／ticket，不把真憑證放 log／產物。設計階段的 SQL／Chromium 模型只證明設計缺口，不能取代產品 integration test。

### 公開 mutation 設計：寫入量與清理上界（#238）
設計 review 對每個無認證、會寫入的程序填資源表：
- 寫哪些表；一次呼叫最多建立幾列；輸入 batch／bytes 上限（在寫入**前**檢查）。
- 限流的預設值與啟用檢查：必要的限制為 0（如 `http.rate_limit.rps=0`）時是拒絕啟用還是靜默關閉？「沿用既有限流」不等於「一定有非零限制」。
- 清理 eligibility、實際清理週期與批次預算；「單列會到期」不等於「總量有界」。
- 測試：從真 entry point 送不存在且無有效簽章的隨機 ID，斷言拒絕／限流後 DB 新增列數有上限；同時保留合法路徑（先取消後 Login、prune 後取消、輪替後取消），避免防 DoS 修正破壞原子性。
- 實作時：config tests 驗證必要限制為 0 會拒絕啟用；provider integration tests 驗證 batch 上限在寫入前檢查。
- 不用任意字串 regex 掃文件取代設計判讀，也不把 SQL 模型當壓測。
- 限流策略由設計者決定，不要求通用 quota framework；硬體吞吐與分散式限流另外量測。

### 為什麼只有 #229 有機器檢查
- #229 的 `check-gen-fresh-all` 重用 CI 既有步驟，成本低，所以做成 task。
- #225／#228／#238 是設計判讀：要驗的內容取決於功能（Login 尚未實作、migration 各有不同鎖、限流策略由設計者定），用 regex 掃文件或 PR body 會產生假綠，只能靠 PR template checkbox 與 reviewer 對照；等相關功能存在時再補確定性測試（見各節）。

## Rename module

```bash
grep -rl 'github.com/blackhorseya/x-monorepo' --include='*.go' --include='go.mod' --include='go.work' \
  --include='buf.gen.yaml' --include='.golangci.yml' --include='Taskfile.yml' . \
  | xargs sed -i '' 's#github.com/blackhorseya/x-monorepo#github.com/<org>/<repo>#g'
task gen wire
```
