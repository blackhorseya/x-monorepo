# Error codes

<sub>← [README](../README.md)</sub>

## Error codes：`x-error-code` 總表

失敗的 RPC 都帶 Connect code（HTTP status 由它決定）；多數另帶 header `x-error-code`，client 有它就用它判斷（`console` 的 `describe()` 就是印它）。沒有 `x-error-code` 的兩種情況（`INTERNAL`、connect 函式庫自己擋下的請求）見下文。
來源三種：**platform interceptor**（`request.*` / `auth.*`）、**admin**（`admin.*`，`pkgs/platform/admin` 自己設）、**domain error**
（`kernel.NewError(kind, code, msg)`，`server.ToConnectError` 依 `Kind` 映射：InvalidArgument → `INVALID_ARGUMENT`、NotFound → `NOT_FOUND`、
FailedPrecondition → `FAILED_PRECONDITION`、Conflict → `ALREADY_EXISTS`、PermissionDenied → `PERMISSION_DENIED`；其他 → `INTERNAL`，**沒有** `x-error-code`，訊息固定 `internal error`）。
connect 函式庫自己擋下的請求沒有 `x-error-code`，只有 Connect code：body 解不開是 `INVALID_ARGUMENT`，單則訊息超過 4 MiB 讀取上限是 `RESOURCE_EXHAUSTED`（`pkgs/platform/server/decode_test.go`、`readlimit_test.go`）。
加新的 domain error 就在該 aggregate 的 `errors.go` 加一行，前綴 = aggregate 名（跨 aggregate 的 BC 層規則用 BC 名）；template 目前沒有 BC，所以表裡只有 platform 與 kernel 的碼；這張表是手寫的，改了記得同步。

| `x-error-code` | Connect code | 誰設 | 什麼時候 |
|---|---|---|---|
| `request.validation_failed` | `INVALID_ARGUMENT` | validate interceptor | request 違反 proto 的 `buf.validate` 規則；`Violations` detail 列欄位（rule id 如 `admin.poison_offsets_xor_all` 也在裡面） |
| `request.timeout` | `DEADLINE_EXCEEDED` | timeout interceptor | 超過 `http.timeout`，不管底下是什麼錯 |
| `request.rate_limited` | `RESOURCE_EXHAUSTED` | rate limit interceptor | 超過 `http.rate_limit`，另帶 `Retry-After` |
| `auth.unauthenticated` | `UNAUTHENTICATED` | auth interceptor、`server.RequireRole` | 沒帶 credential：`auth.mode=required`；或 `auth.mode=optional` 以 anonymous 進來、卻走到 `server.RequireRole` |
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了 credential 而驗不過，或 resolver 查詢失敗、逾時。帶了 `X-API-Key` 時不看 Bearer：key 對不上（含 resolver 回 not found）就是這個碼，不會 fallback。`auth.jwt` 的 Bearer 驗證失敗（簽章、`exp` 不對，缺 `sub`；設了 `auth.jwt.issuer`／`audience` 時含 `iss`／`aud`）也是，訊息帶驗證失敗的原因。接了 `server.CredentialResolver`、或 `auth.oidc` 開著的衍生專案（template 的 rpc 兩者都沒接，`auth.oidc.enabled` 會被拒絕啟動），client 看到固定訊息、原因只進 server log：resolver 查詢失敗是 `credential could not be verified`；`iss` 是已設定的 OIDC issuer 的 token 驗不過、或帳號被 resolver 拒絕（`ErrCredentialRefused`）是 `token could not be verified`。`auth.oidc` 開著時 `iss` 不是任何已設定 issuer 的 Bearer 走 `auth.jwt`（沒設 `auth.jwt` 就被拒，訊息 `bearer tokens are not accepted`），見 [auth.md](auth.md#憑證與優先序)。`auth.resolver_timeout` 大於 0 時，逾時後才回來的答案被拒，連「未綁定」也不例外，不會放行 `AllowsUnbound` 的 procedure |
| `auth.unbound_identity` | `PERMISSION_DENIED` | auth interceptor | OIDC token 驗過了，`ExternalIdentityResolver` 回 `ErrUnboundIdentity`（`(iss, sub)` 沒對到任何 account），而 resolver 的 `AllowsUnbound` 不放行這個 procedure。未綁定的身分呼叫 `AllowsUnbound` 不放行的 procedure 會被拒（`auth.mode=optional` 也一樣，不會變 anonymous）；template 沒有接 resolver，這條碼要接上之後才會出現 |
| `auth.permission_denied` | `PERMISSION_DENIED` | `server.RequireRole` | 有身分但缺角色（AdminService 要 `auth.admin_role`；其他 procedure 在 usecase 或 `server.RequireRole` 檢查） |
| `concurrent_update` | `ABORTED` | `server.ToConnectError` | optimistic lock 撞版（aggregate 的 `ErrStale`，wrap `kernel.ErrConcurrentUpdate`）；重送即可，刻意沒有前綴也不是 `kernel.Error` |
| `pagination.invalid_page_size` | `INVALID_ARGUMENT` | `platform/pagination` | `page_size` 負數 |
| `pagination.invalid_token` | `INVALID_ARGUMENT` | `platform/pagination` | `page_token` 解不開，或帶的 filter 跟這次 request 不同（filter 一併編進 token 的列表才有這一條；目前沒有帶 filter 的 RPC） |
| `admin.no_database` | `FAILED_PRECONDITION` | admin | server 沒接 Postgres（`postgres.url` 空）：poison 表與 outbox / inbox 都在 DB |
| `admin.unknown_bounded_context` | `INVALID_ARGUMENT` | admin | `bounded_context` 不在 `apps/rpc` 列的 schema 清單 |
| `admin.poison_not_found` | `NOT_FOUND` | admin | `GetPoison` 的 offset 不存在 |
| `money.invalid_amount` / `money.invalid_currency` | `INVALID_ARGUMENT` | kernel `Money` | 金額為負、幣別不是 3 個大寫英文字母（ISO 4217 形狀） |
| `message.duplicate` | （不到 rpc） | `usecasex.Idempotent` | consumer inbox 看到重送的 message id：handler 回它、broker ack、不 retry；rpc path 沒 message id 不會發 |

Kind → Connect code 只在 `server.ToConnectError` 一處；consumer 不看 code：`kernel.Error` 一律 drop（進 poison），其他 error retry。
