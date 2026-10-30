# CI 與 gates

<sub>← [README](../README.md)</sub>

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build、helm（`task helm-check`）、helm-kind（matrix 兩個平行 leg：`task helm-kind` kind 真裝 + smoke、`task helm-kind-keda` KEDA 擴縮，各自一個 cluster）、k6（`task k6-smoke`：load / rate limit / timeout / reservation（rpc + worker）四個 k6 腳本打真的 rpc + Postgres）、release-snapshot、console-e2e / console-image、actions-resolve、scripts-test（`scripts/test.sh`：ci-changes / releasable / next-version / release-gate-pr 四支決策腳本，加 merge-ready（stub gh）與 merge-tree-check，對拋棄式 git repo 的測試——規則寫錯會靜默跳過 job，所以釘住）；`buf breaking` **只警告**。PR 上每個 job 只在它的輸入有變時跑（`scripts/ci-changes.sh`：Go / proto → Go 系 + kind + k6；`web/` → web + e2e + image + kind；`deployments/helm/` → helm + kind；`.github/` → actions-resolve；ci.yml / `Taskfile.yml` / `scripts/` → 全部；`deployments/helm/kind/coredns-dex.sh`、`.github/workflows/k6-bench.yml`、`.goreleaser.yaml`（`scripts/test.sh` 執行或讀取它們）另外觸發 scripts-test；docs / Grafana / compose → 幾乎沒有），**main 上一律全跑**（它是 release 的 gate） | 依改動 |
| `.github/workflows/k6-bench.yml` | 只有 `workflow_dispatch`（沒有 input） | 乾淨 GitHub-hosted runner 上跑 `task k6-bench`（效能 baseline，#280）：Postgres service container、runner 規格寫進 `runner.txt` 與 step summary；bench.sh exit 0 才綠，exit 3（valid attempt 不足，NOT A VALID BASELINE）或其他失敗一律紅並帶 error annotation；raw 結果與 summary 不論成敗都上傳 artifact。不進 PR / main check、不自動寫回 baseline（另開 docs PR）；判讀見 [perf-baseline.md](perf-baseline.md) | 手動 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/ci-changes.sh`：由改動的路徑算出 ci.yml 每個 job 的 `run_<job>` 旗標（`affected` job 的 outputs，其餘 job 都 `if:` 它）；本機 `scripts/ci-changes.sh` 看這支 branch 會跑哪些，`CI_ALL=1` 是 main 的行為；對照表由 `scripts/test.sh`（`task scripts-test`）釘住。維護規則：`scripts/test.sh` 新增讀取或執行 `scripts/` 以外的檔案時，要同步把該路徑加進 `ci-changes.sh` 的 `scripts_src`，並在 `test.sh` 補只改該路徑的 sole-path 案例，否則只改那個檔案的 PR 不會跑 scripts-test
- `scripts/suite-guard.sh`（#307）：`scripts/test.sh` 一開始就用它重新執行自己——套件跑在**自己的 process group**，結束時（正常、有案例失敗、被 INT／TERM 打斷都算）檢查該 group 還有沒有活著的 process（stub server 等背景 fixture）：有就印出數量與指令、先 TERM 再 KILL 並等它們結束，並讓套件失敗（原本是 0 就改成 1；原本就失敗則保留原退出碼）。收到 INT／TERM 時先對 group 送 TERM，套件若忽略它就在 `SUITE_GUARD_GRACE`（預設 5）秒後對整個 group 送 KILL，守衛不會被不肯結束的套件拖住；只動自己建立的 group，不掃描也不殺別的行程；套件自己的 EXIT trap 管不到沒登記的 fixture、`set -e` 提前中止、SIGKILL，這層才是保證。守衛本身由 `test.sh` 的 `guard:` 案例釘住（假套件留下背景行程：正常／失敗／忽略 TERM，以及 INT／TERM 中斷，另外確認 group 外的行程不受影響）。
- `scripts/merge-ready.sh <pr>`（`task merge-ready -- <pr>`）：**merge 前必跑**。`pull_request` run 驗的是 `refs/pull/N/merge`——PR head 合進「run 開始當下的 main」；main 之後前進，這顆綠燈就不再是 main + 這個 PR 的結果（#187 改了 `NewCreateOrder` 簽章、#188 新測試還用舊簽章：各自綠、git 無衝突、合起來編譯失敗）。腳本取該 head 最新的 ci.yml run、找出它的 merge commit，比對 merge commit 的 base parent 與 main 現在的 HEAD、head parent 與 PR head；並確認 run 已完成、check 齊全且無 pending / fail / cancel。輸出 main / merge / base / head 四個 SHA 與 check 數。exit 0 可合、1 check 未完成或失敗、2 stale（不可合，先重驗）、3 用法錯
  - **stale 的重驗**：`task merge-ready -- <pr> --revalidate` 派送 `.github/workflows/merge-tree.yml`（`workflow_dispatch`，input 為 pr / head / main SHA），用 `scripts/merge-tree-check.sh` 在記憶體裡 `git merge-tree` 合 main + head、匯出 tree、對每個 go.work module 跑 `go vet ./...`（含測試編譯）；workflow 第一步（任何 setup / 以 base checkout 之前）用 `scripts/merge-tree-inputs.sh` 驗輸入（pr 純數字、head / base 為 40 位小寫 hex，後綴或空白一律拒絕），checkout 後再 assert `HEAD == base`，所以成功 run 的標題 `pr= head= base=` 就是實際 build 的 SHA；下一次 merge-ready 以**完整等值**比對標題（不是子字串）與 completed / success，只放行「恰好是現在的 main 與 head」的成功 run。**不要用 `gh run rerun`**：實測 rerun 沿用原本的 merge commit（#188 在 main 前進後 rerun，attempt 2 仍是同一個 merge SHA、base 仍是舊 main），等於重跑同一棵 stale tree；也**不要 update branch**：PR head 一變就要重審。`merge-tree.yml` 要在 default branch 上才能派送
  - 本機也能直接跑：`scripts/merge-tree-check.sh origin/main origin/<branch>`（git >= 2.38；0 可編譯、1 不能編譯、2 git 文字衝突）
  - `scripts/test.sh` 釘住它：stub 掉 `gh` 測 fresh / stale / head 不符 / pending / fail / 無 run / `--revalidate` 不用 rerun；另用兩條本機分支（一條改 constructor、一條新增舊呼叫）證明「git 無衝突、各自可編譯、合成 tree 編譯失敗」會被抓到
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go bump 會讓其他 module 漂移，`dependabot-sync.yml` 在它的分支跑 `task tidy` 補 commit 再觸發 CI，否則 `work-sync` 會擋。`.github/dependabot.yml` 三組每週各一支 PR：gomod（所有 module 一組）、github-actions、npm（`web/` pnpm workspace 一組；lockfile 算 releasable，merge 會切 patch 版）

## 本機 hook 證據：AgEnD hooks ≠ lefthook（#300）

`git commit` / `git push` 成功，不代表 `lefthook.yml` 的 `gate-fast` / `gate-local` 跑過：git 只執行 active `core.hooksPath` 底下**同名**的 hook。AgEnD 管理的 worktree 會把 `core.hooksPath` 指向 `~/.agend/hooks`，那裡只有 `prepare-commit-msg`、`reference-transaction` 等 AgEnD hooks，沒有 `pre-commit` / `pre-push`，也不呼叫 lefthook。`task tools:hooks` 裝的 lefthook 因此不會被觸發。

### 1. 先驗證 hook 有沒有真的掛上

```bash
git config --show-origin --get core.hooksPath   # 空 = 預設 .git/hooks；有值 = 看來源檔與路徑
ls -l "$(git rev-parse --git-path hooks)"       # 預設路徑
ls -l "$(git config --get core.hooksPath)"      # 有設 hooksPath 時看這個
```

要看到 `pre-commit` / `pre-push` 且內容呼叫 lefthook（`grep -l lefthook <hook>`），才能說 lefthook 掛上。找不到就視為**沒掛上**。AgEnD 的 hooks 與 Claude Code 的 `.claude/settings.json` hooks（`PostToolUse` / `Stop`）也各自獨立，不能互相當證據。

### 2. 沒掛上時：在 exact head 手動跑門檻

在要回報的 commit（`git rev-parse HEAD`）上跑 `task gate-local`（commit 前的檔案檢查用 `task gate-fast FILE=…`），回報要留命令、exit code 與輸出摘要。措辭固定：

| 情況 | 可以寫 | 不可以寫 |
|---|---|---|
| lefthook 掛上，hook 跑過（有 hook 輸出） | lefthook 通過 | — |
| 沒掛上，手動跑 gate 且 exit 0 | **手動 gate 通過，hook 未執行**（附 head SHA、命令、exit code） | lefthook 通過、hooks 通過 |
| 沒掛上，沒跑 gate | 本機 gate 未執行 | 任何「通過」 |

範例：

```text
head: 0b95a126…（git rev-parse HEAD）
core.hooksPath: /Users/sean/.agend/hooks（file:…/.git/config）；hooks 目錄無 pre-commit / pre-push
$ task gate-local → exit 0（check-gen-fresh 乾淨、buf lint 乾淨、affected modules go test -race ok）
結論：手動 gate 通過，hook 未執行
```

### 3. 每個 head 各自驗證

merge commit、rebase 後或任何新 head 都要在**該 head** 重驗，不沿用 parent 的結果。回報裡的 SHA 要與最後 push 的 head 一致。

### 4. 已由 exact-head CI 證明的不必重跑

CI 在 `refs/pull/N/merge` 上跑（見 `task merge-ready`），綠燈對得上目前 head 時，下列門檻不必本機重複：gen-freshness（含 TS）、`task lint`、work-sync、`go test -race`（test matrix）、coverage、build、helm、k6 / helm-kind、release-snapshot、web。哪些 job 實際執行取決於改動路徑（`scripts/ci-changes.sh`）；被跳過的 job 不是證據。本機手動 gate 的價值在於「push 前就知道」，**不取代** CI；CI 也無法證明本機 hook 曾經執行。

### 5. 不要繞過 hook

不用 `-c core.hooksPath=/dev/null`、`--no-verify` 跳過 hook，除非 lead 明確授權；授權時要在回報裡寫明被跳過的 commit 與理由。也不要為了讓 lefthook 跑起來去改全域 `core.hooksPath` 或 `~/.agend/hooks`（影響其他 worktree 與 daemon 保護）；缺 hook 就走第 2 點手動跑。

### Claude-Session trailer

repo 沒有這個規範：`CLAUDE.md`、`docs/`、`.github/`、`scripts/` 都沒有 `Claude-Session`，近 60 筆 commit 也沒有；commit 規則只有 Conventional Commits。#297 提到的 trailer 來自當次派工 brief，不是 repo 要求。AgEnD 的 `prepare-commit-msg` 注入的是 `Agend-*` 與選用的 `Signed-off-by`，且 hook 失敗不擋 commit。
