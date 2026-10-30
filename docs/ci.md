# CI 與 gates

<sub>← [README](../README.md)</sub>

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build、helm（`task helm-check`）、helm-kind（matrix 兩個平行 leg：`task helm-kind` kind 真裝 + smoke、`task helm-kind-keda` KEDA 擴縮，各自一個 cluster）、k6（`task k6-smoke`：load / rate limit / timeout / reservation（rpc + worker）四個 k6 腳本打真的 rpc + Postgres）、release-snapshot、console-e2e / console-image、actions-resolve、scripts-test（`scripts/test.sh`：ci-changes / releasable / next-version / release-gate-pr 四支決策腳本，加 merge-ready（stub gh）與 merge-tree-check，對拋棄式 git repo 的測試——規則寫錯會靜默跳過 job，所以釘住）；`buf breaking` **只警告**。PR 上每個 job 只在它的輸入有變時跑（`scripts/ci-changes.sh`：Go / proto → Go 系 + kind + k6；`web/` → web + e2e + image + kind；`deployments/helm/` → helm + kind；`.github/` → actions-resolve；ci.yml / `Taskfile.yml` / `scripts/` → 全部；docs / Grafana / compose → 幾乎沒有），**main 上一律全跑**（它是 release 的 gate） | 依改動 |
| `.github/workflows/k6-bench.yml` | 只有 `workflow_dispatch`（沒有 input） | 乾淨 GitHub-hosted runner 上跑 `task k6-bench`（效能 baseline，#280）：Postgres service container、runner 規格寫進 `runner.txt` 與 step summary；bench.sh exit 0 才綠，exit 3（valid attempt 不足，NOT A VALID BASELINE）或其他失敗一律紅並帶 error annotation；raw 結果與 summary 不論成敗都上傳 artifact。不進 PR / main check、不自動寫回 baseline（另開 docs PR）；判讀見 [perf-baseline.md](perf-baseline.md) | 手動 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/ci-changes.sh`：由改動的路徑算出 ci.yml 每個 job 的 `run_<job>` 旗標（`affected` job 的 outputs，其餘 job 都 `if:` 它）；本機 `scripts/ci-changes.sh` 看這支 branch 會跑哪些，`CI_ALL=1` 是 main 的行為；對照表由 `scripts/test.sh`（`task scripts-test`）釘住
- `scripts/merge-ready.sh <pr>`（`task merge-ready -- <pr>`）：**merge 前必跑**。`pull_request` run 驗的是 `refs/pull/N/merge`——PR head 合進「run 開始當下的 main」；main 之後前進，這顆綠燈就不再是 main + 這個 PR 的結果（#187 改了 `NewCreateOrder` 簽章、#188 新測試還用舊簽章：各自綠、git 無衝突、合起來編譯失敗）。腳本取該 head 最新的 ci.yml run、找出它的 merge commit，比對 merge commit 的 base parent 與 main 現在的 HEAD、head parent 與 PR head；並確認 run 已完成、check 齊全且無 pending / fail / cancel。輸出 main / merge / base / head 四個 SHA 與 check 數。exit 0 可合、1 check 未完成或失敗、2 stale（不可合，先重驗）、3 用法錯
  - **stale 的重驗**：`task merge-ready -- <pr> --revalidate` 派送 `.github/workflows/merge-tree.yml`（`workflow_dispatch`，input 為 pr / head / main SHA），用 `scripts/merge-tree-check.sh` 在記憶體裡 `git merge-tree` 合 main + head、匯出 tree、對每個 go.work module 跑 `go vet ./...`（含測試編譯）；workflow 第一步（任何 setup / 以 base checkout 之前）用 `scripts/merge-tree-inputs.sh` 驗輸入（pr 純數字、head / base 為 40 位小寫 hex，後綴或空白一律拒絕），checkout 後再 assert `HEAD == base`，所以成功 run 的標題 `pr= head= base=` 就是實際 build 的 SHA；下一次 merge-ready 以**完整等值**比對標題（不是子字串）與 completed / success，只放行「恰好是現在的 main 與 head」的成功 run。**不要用 `gh run rerun`**：實測 rerun 沿用原本的 merge commit（#188 在 main 前進後 rerun，attempt 2 仍是同一個 merge SHA、base 仍是舊 main），等於重跑同一棵 stale tree；也**不要 update branch**：PR head 一變就要重審。`merge-tree.yml` 要在 default branch 上才能派送
  - 本機也能直接跑：`scripts/merge-tree-check.sh origin/main origin/<branch>`（git >= 2.38；0 可編譯、1 不能編譯、2 git 文字衝突）
  - `scripts/test.sh` 釘住它：stub 掉 `gh` 測 fresh / stale / head 不符 / pending / fail / 無 run / `--revalidate` 不用 rerun；另用兩條本機分支（一條改 constructor、一條新增舊呼叫）證明「git 無衝突、各自可編譯、合成 tree 編譯失敗」會被抓到
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go bump 會讓其他 module 漂移，`dependabot-sync.yml` 在它的分支跑 `task tidy` 補 commit 再觸發 CI，否則 `work-sync` 會擋。`.github/dependabot.yml` 三組每週各一支 PR：gomod（所有 module 一組）、github-actions、npm（`web/` pnpm workspace 一組；lockfile 算 releasable，merge 會切 patch 版）
