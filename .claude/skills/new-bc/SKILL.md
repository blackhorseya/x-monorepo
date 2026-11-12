---
name: new-bc
description: 在這個 monorepo 新增一個 bounded context（pkgs/<name>、protos/<name>、migration，並在每個共用檔加一筆）。要新增 BC、要幫 BC 加 event（consumer／outbox／inbox／poison）、或 `task new-bc` 失敗要還原時使用。
---

# 新增 Bounded Context

## 何時用
新增一個 `pkgs/<name>`。不是改既有 BC（那是 `CLAUDE.md` 的「加一個 RPC」）。

## 指令
```bash
task new-bc -- <name>            # 沒有 event 的 BC
task new-bc -- <name> --events   # 有 event：outbox、同 BC 的 consumer、inbox、poison、worker 訂閱
```
順序固定是：產生器 → `task gen` → `scripts/sync-fixpoint.sh`（`go mod tidy` + `go work sync`，跑到沒有 diff，#475）→ `task wire`。
`tidy` 一定排在 `gen` 後面：gen 還沒寫出 connect package 時 tidy 會失敗。

## 名稱規則（錯了會說是哪一條）
- `scripts/bcs.jq`：`^[a-z][a-z0-9]*$`（`BCS` 用它，產生器直接 exec `jq` 跑它；`MODULES` 用的是另一份 `scripts/modules.jq`，白名單不同）
- 最長 30 字元（名稱是 Postgres schema，也是 `events_<topic>` 表名的一段）
- 不是 Go keyword，不是 `kernel`、`platform`、`main`
- `pkgs/<name>` 或 `protos/<name>` 已存在 → 拒絕

## 產生器做什麼
- 新檔：`protos/<name>/v1/*`、`pkgs/<name>/**`（一個 aggregate，Create 與 Get；memory 與 postgres repository；`arch_test.go`、`pgtest_cleanup_test.go`〔`pkgs/platform/archtest` 要求每個 module 都有〕；use case 與 domain 的測試）、`0001_<name>` migration。
  `--events` 另加 `<Name>Created` event（經 outbox 發）、同 BC 的 consumer（經 inbox，只記「已處理」）、`0002_events`、`0003_inbox`。
- 共用檔各加一筆：`go.work`、`buf.gen.yaml`、`apps/{rpc,migrate}/go.mod`、`apps/rpc/{wire.go,app.go}`、`apps/migrate/main.go`；`--events` 另加 `apps/worker/{go.mod,wire.go,consumers.go,poison.go}`。
- 插入點是共用檔裡的 `new-bc:*` 錨點註解（整行比對、必須剛好一個），插入後用 parser 驗過。**不要刪錨點，也不要複製它**；少了或重複，產生器會在寫任何檔之前停下。
- 全部內容先在記憶體算完、驗完才寫；被拒絕時零個檔案變動。

## 產生完還要手動做
1. `task gen-ts`（要網路）：TS client 進 `web/packages/proto/src/gen/<name>/`，CI 的 gen-freshness 也比這個。
2. 把樣板的 aggregate、proto 欄位與 migration 改成真正的領域；`openapi.base.yaml` 的描述也改。
3. 把 `task tidy` 的結果一起 commit（`task check-work-sync` 會比）。
4. `--events` 的 BC：確認 `apps/worker/poison.go` 的 `schemas` 有它、`RPC.PoisonSchema` 有填、`worker prune` 的 retention 涵蓋它的 inbox／outbox。

## 驗證
```bash
task new-bc-check
```
在工作樹的**複本**（`git ls-files -co --exclude-standard`：含尚未 `git add` 的新檔，不含 ignore 的檔）產生 `cisample` 與 `cisampleevents --events`，跑 gen、tidy、wire、vet、lint、測試、`scripts/cover-gate.sh`（兩個範例的 `domain` package 都必須 100% 覆蓋）與三個 app 的 build、`TestArchitecture`。除了被 ignore 的 `bin/` 下的 coverage profile（`bin/cover-*.out`：複本的 `bin` 是指向你工作樹 `bin` 的 symlink，`scripts/new-bc-check.sh` 的 `ln -s`；`scripts/cover-gate.sh` 先 `cd` 到複本根目錄、`mkdir -p bin`，再用 `go test -coverprofile=bin/cover-….out` 寫入）之外，不寫任何東西到你的工作樹；CI 的 gen-freshness 也跑它。
**它不跑 postgres repository 的測試**：gen-freshness 沒有 DB，產生的測試只用 memory 的 infra。postgres 的程式碼要在有 DB 的環境（`task compose-up`、`task migrate -- up`）自己驗。

## 失敗與還原
產生器寫到一半失敗，訊息會列出已寫的檔。還原：
```bash
git restore .                              # 被改的共用檔
git clean -fd pkgs/<name> protos/<name>    # 新檔
```
先看 `git status`，確認工作樹沒有你要留的其他改動再 `git restore .`。`gen`、`tidy`、`wire` 之後失敗時，已產生的檔還在：修好原因重跑該步驟即可，不要重跑產生器（它會因為 `pkgs/<name>` 已存在而拒絕）。

## 注意
- `task migrate -- down` 讓**每個** BC 各退一步，資料跟著表丟掉：`--events` 的 BC 最新一版是 `0003_inbox`（丟 inbox 表），再退一步是 `0002_events`（丟 events 與 poison 表，未處理的 event 與 poison 的 message 都沒了）。不要把它當單一 BC 的 rollback。
- **`--events` 的 BC 需要 Postgres**（outbox、inbox 都在 DB）。`apps/worker` 在 `postgres.url` 為空時拒絕啟動：`postgres.url is required: the event tables are the outbox the worker reads or forwards`（`apps/worker/main.go:63-65`）。
- 產生的是起點，不是成品：沒有 List、沒有 stats projection、沒有跨 BC 的 consumer。需要時照 `docs/workflow.md` 加。
