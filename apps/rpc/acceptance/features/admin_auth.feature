# language: zh-TW
功能: 管理端 RPC 的存取控制
  admin.v1 在 auth.mode=required 下，只有帶 admin 權限 credential 的呼叫才會到達 handler。

  場景: 帶 admin API key 呼叫，授權成功並由 handler 回應
    假如 rpc 以 auth.mode=required 啟動，資料庫為該場景獨立的 DB
    當 以 admin API key 呼叫 ListPoison
    那麼 呼叫成功，回應是沒有任何 poison 訊息的清單
    而且 指定不存在的 bounded context 時，由 handler 回 invalid_argument 與 x-error-code "admin.unknown_bounded_context"

  場景: 不帶 credential 呼叫，被拒絕
    假如 rpc 以 auth.mode=required 啟動，資料庫為該場景獨立的 DB
    當 不帶任何 credential 呼叫 ListPoison
    那麼 呼叫被拒絕，Connect code 為 "unauthenticated"，x-error-code 為 "auth.unauthenticated"

  場景: 已驗證但缺權限，被拒絕
    假如 rpc 以 auth.mode=required 啟動，資料庫為該場景獨立的 DB
    當 以沒有任何權限的 API key 呼叫 ListPoison
    那麼 呼叫被拒絕，Connect code 為 "permission_denied"，x-error-code 為 "auth.permission_denied"
