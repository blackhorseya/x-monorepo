## 變更

<!-- 做了什麼、為什麼。Closes #N -->

## 驗證

<!-- 指令與結果；行為變更的 RED evidence 見 CLAUDE.md「Conventions」 -->

## Review contracts

不適用的項目填 `N/A` 與一句理由，不要直接刪掉。規則全文與出處：[`docs/workflow.md`](../docs/workflow.md#review-contracts)。

- [ ] **proto 變更**：驗證回報逐項列 Go、OpenAPI、Wire、TS 的指令與結果；TS 以 `task check-gen-fresh-all`（或 `task gen wire gen-ts` + `git diff --exit-code`）為證，`task check-gen-fresh` 不含 TS
- [ ] **migration 的可用性宣稱**（文件／註解寫「線上遷移期間讀／寫可用」）：已用完整 migration 交易做雙連線驗證（session A 跑完整 migration 不 commit，session B 看 `pg_locks` 並以短 `lock_timeout` 做代表性 SELECT／寫入），結果附在此
- [ ] **認證／session 的 generation、取消類測試**：已列副作用界線表（JS state、Web Storage、HttpOnly cookie、server session）；真 Console + 真 entry point 的 browser 測試在 reload 後同時斷言 cookie 與 server session，並含失敗路徑、成功取消控制組與「正常 Login → reload → Refresh 成功」正例
- [ ] **公開（無認證）mutation 設計**：已填資源表——寫哪些表、單次最多幾列、輸入 batch／bytes 上限、非零限流及啟用檢查、清理 eligibility 與週期／批次預算
