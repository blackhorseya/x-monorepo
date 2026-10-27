# CI 與 gates

<sub>← [README](../README.md)</sub>

## Gates（harness）

單一事實來源是 `Taskfile.yml`，Claude Code hooks、lefthook、CI 都只是呼叫它：

| Gate | 何時 | 跑什麼 | 時間 |
|---|---|---|---|
| `task gate-fast FILE=…` | Claude Code `PostToolUse`（Edit/Write）、`pre-commit` | gofmt + vet + golangci-lint（含 depguard）+ archtest（module 根有 `arch_test.go` 時）於被改檔案所屬 module | 秒 |
| `task gate-local` | Claude Code `Stop`、`pre-push` | gen freshness + buf lint + affected modules 的 `go test -race`（含 archtest） | ~1 min |
| `.github/workflows/ci.yml` | PR | gen-freshness（含 TS）、lint、work-sync、test（matrix by affected module）、coverage、build、helm（`task helm-check`）、helm-kind（matrix 兩個平行 leg：`task helm-kind` kind 真裝 + smoke、`task helm-kind-keda` KEDA 擴縮，各自一個 cluster）、k6（`task k6-smoke`：load / rate limit / timeout / reservation（rpc + worker）四個 k6 腳本打真的 rpc + Postgres）、release-snapshot、console-e2e / console-image、actions-resolve、scripts-test（`scripts/test.sh`：ci-changes / releasable / next-version / release-gate-pr 四支決策腳本對拋棄式 git repo 的測試——規則寫錯會靜默跳過 job，所以釘住）；`buf breaking` **只警告**。PR 上每個 job 只在它的輸入有變時跑（`scripts/ci-changes.sh`：Go / proto → Go 系 + kind + k6；`web/` → web + e2e + image + kind；`deployments/helm/` → helm + kind；`.github/` → actions-resolve；ci.yml / `Taskfile.yml` / `scripts/` → 全部；docs / Grafana / compose → 幾乎沒有），**main 上一律全跑**（它是 release 的 gate） | 依改動 |

- `scripts/affected.sh`：由 `go.work` 算出被改動的 module，並展開反向依賴（改 kernel → 全部；改 ordering → ordering + apps/rpc）
- `scripts/ci-changes.sh`：由改動的路徑算出 ci.yml 每個 job 的 `run_<job>` 旗標（`affected` job 的 outputs，其餘 job 都 `if:` 它）；本機 `scripts/ci-changes.sh` 看這支 branch 會跑哪些，`CI_ALL=1` 是 main 的行為；對照表由 `scripts/test.sh`（`task scripts-test`）釘住
- `scripts/cover-gate.sh`：所有 `*/domain/*` package 必須 100% 覆蓋
- hook 失敗時 exit 2，stderr 會回到 agent 讓它自己修；`stop_hook_active` 防止無限循環
- dependabot 只掃單一 module 目錄、不懂 go.work：Go bump 會讓其他 module 漂移，`dependabot-sync.yml` 在它的分支跑 `task tidy` 補 commit 再觸發 CI，否則 `work-sync` 會擋。`.github/dependabot.yml` 三組每週各一支 PR：gomod（所有 module 一組）、github-actions、npm（`web/` pnpm workspace 一組；lockfile 算 releasable，merge 會切 patch 版）
