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
| `auth.unauthenticated` | `UNAUTHENTICATED` | auth interceptor | `auth.mode=required` 且沒帶 credential |
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了但對不上（未知 API key、JWT 簽章 / exp / iss / aud 不對）。接了 `server.CredentialResolver` 的衍生專案：key 被 resolver 拒絕、或 resolver 查詢失敗，client 看到的也是這個（不洩漏原因；resolver 失敗另有 error log）。OIDC token（`auth.oidc`，template 的 rpc 沒接 resolver，啟動即被拒絕）同理：簽章 / `iss` / `aud` / `exp` / 缺 `iat` 不對、resolver 拒絕、`(iss, sub)` 查詢失敗，一律這個碼加固定訊息（token 本身或帳號被拒：`token could not be verified`；resolver 查不到答案：`credential could not be verified`，原因只進 server log，且 log 不含 token）。逾時（`auth.resolver_timeout`）後才回來的答案一律拒絕，連「未綁定」也不例外，不會放行 `AllowsUnbound` 的 procedure。帶了 `X-API-Key` 時不看 Bearer，key 錯就是這個，不會 fallback |
| `auth.unbound_identity` | `PERMISSION_DENIED` | auth interceptor | OIDC token 驗過了，但 `(iss, sub)` 沒綁定任何 account（`mapping: bound`），而 resolver 的 `AllowsUnbound` 不放行這個 procedure。未綁定的身分只能呼叫 `AllowsUnbound` 放行的 procedure（`auth.mode=optional` 也一樣，不會變 anonymous）；template 沒有接 resolver，這條碼要接上之後才會出現 |
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
