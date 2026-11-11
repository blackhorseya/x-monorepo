# Error codes

<sub>← [README](../README.md)</sub>

## Error codes：`x-error-code` 總表

失敗的 RPC 都帶 Connect code（HTTP status 由它決定）；多數另帶 header `x-error-code`，client 有它就用它判斷（`console` 的 `describe()` 就是印它）。沒有 `x-error-code` 的兩種情況（`INTERNAL`、connect 函式庫自己擋下的請求）見下文。
來源三種：**platform interceptor**（`request.*` / `auth.*`）、**admin**（`admin.*`，`pkgs/platform/admin` 自己設）、**domain error**
（`kernel.NewError(kind, code, msg)`，`server.ToConnectError` 依 `Kind` 映射：InvalidArgument → `INVALID_ARGUMENT`、NotFound → `NOT_FOUND`、
FailedPrecondition → `FAILED_PRECONDITION`、Conflict → `ALREADY_EXISTS`、PermissionDenied → `PERMISSION_DENIED`；其他 → `INTERNAL`，**沒有** `x-error-code`，訊息固定 `internal error`）。
connect 函式庫自己擋下的請求沒有 `x-error-code`，只有 Connect code：body 解不開是 `INVALID_ARGUMENT`，單則訊息超過 4 MiB 讀取上限是 `RESOURCE_EXHAUSTED`（`pkgs/platform/server/decode_test.go`、`readlimit_test.go`）。
加新的 domain error 就在該 aggregate 的 `errors.go` 加一行，前綴 = aggregate 名（跨 aggregate 的 BC 層規則用 BC 名，如 `identity.last_admin`）；這張表是手寫的，改了記得同步。

| `x-error-code` | Connect code | 誰設 | 什麼時候 |
|---|---|---|---|
| `request.validation_failed` | `INVALID_ARGUMENT` | validate interceptor | request 違反 proto 的 `buf.validate` 規則；`Violations` detail 列欄位（rule id 如 `admin.poison_offsets_xor_all` 也在裡面） |
| `request.timeout` | `DEADLINE_EXCEEDED` | timeout interceptor | 超過 `http.timeout`，不管底下是什麼錯 |
| `request.rate_limited` | `RESOURCE_EXHAUSTED` | rate limit interceptor | 超過 `http.rate_limit`，另帶 `Retry-After` |
| `auth.unauthenticated` | `UNAUTHENTICATED` | auth interceptor | `auth.mode=required` 且沒帶 credential |
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了但對不上（未知 API key、JWT 簽章 / exp / iss / aud 不對）。identity 的 key 被 revoke / 過期 / account disabled、或 resolver 查 DB 失敗，client 看到的也是這個（不洩漏原因；resolver 失敗另有 error log）。OIDC token（`auth.oidc`）同理：簽章 / `iss` / `aud` / `exp` / 缺 `iat` 不對、對應的 account 已停用、`iat` 早於 `sessions_revoked_at`、`(iss, sub)` 查 DB 失敗，一律這個碼加固定訊息（token 本身或帳號被拒：`token could not be verified`；resolver 查不到答案：`credential could not be verified`，原因只進 server log，且 log 不含 token）。逾時（`auth.resolver_timeout`）後才回來的答案一律拒絕，連「未綁定」也不例外，不會放行 `ClaimInvite`。帶了 `X-API-Key` 時不看 Bearer，key 錯就是這個，不會 fallback |
| `auth.unbound_identity` | `PERMISSION_DENIED` | auth interceptor | OIDC token 驗過了，但 `(iss, sub)` 沒綁定任何 account（`mapping: bound`），而這個 procedure 不是 `IdentityService.ClaimInvite`。未綁定的身分只能呼叫 `ClaimInvite`（`auth.mode=optional` 也一樣，不會變 anonymous）。要先請 admin 發邀請，再用 `ClaimInvite` 綁定 |
| `auth.permission_denied` | `PERMISSION_DENIED` | `server.RequireRole` | 有身分但缺角色（AdminService、`IdentityService` 除 `WhoAmI` 與 `ClaimInvite` 外都要 `auth.admin_role`） |
| `concurrent_update` | `ABORTED` | `server.ToConnectError` | optimistic lock 撞版（`account.ErrStale` / `apikey.ErrStale` / `invite.ErrStale`，wrap `kernel.ErrConcurrentUpdate`）；重送即可，刻意沒有前綴也不是 `kernel.Error` |
| `pagination.invalid_page_size` | `INVALID_ARGUMENT` | `platform/pagination` | `page_size` 負數 |
| `pagination.invalid_token` | `INVALID_ARGUMENT` | `platform/pagination` | `page_token` 解不開（`ListAccounts` 就會回這個），或帶的 filter 跟這次 request 不同（filter 一併編進 token 的列表才有這一條；目前沒有帶 filter 的 RPC） |
| `admin.no_database` | `FAILED_PRECONDITION` | admin | server 沒接 Postgres（`postgres.url` 空）：poison 表與 outbox / inbox 都在 DB |
| `admin.unknown_bounded_context` | `INVALID_ARGUMENT` | admin | `bounded_context` 不在 `apps/rpc` 列的 schema 清單 |
| `admin.poison_not_found` | `NOT_FOUND` | admin | `GetPoison` 的 offset 不存在 |
| `money.invalid_amount` / `money.invalid_currency` | `INVALID_ARGUMENT` | kernel `Money` | 金額為負、幣別不是 3 個大寫英文字母（ISO 4217 形狀） |
| `account.invalid_id` / `account.invalid_name` / `account.invalid_role` | `INVALID_ARGUMENT` | identity domain | aggregate 不變量（role 不能是空字串；protovalidate 先擋 rpc 那條） |
| `account.invalid_cursor` | `INVALID_ARGUMENT` | identity domain | ListAccounts 的 page token 解得開但沒有 id |
| `account.already_disabled` / `account.already_active` | `FAILED_PRECONDITION` | identity domain | Disable 已 DISABLED / Enable 已 ACTIVE 的 account |
| `identity.last_admin` | `FAILED_PRECONDITION` | identity domain（`account.GuardLastAdmin` / `GuardLastAdminKey`） | `DisableAccount` / `AssignRoles`（拿掉 `auth.admin_role`）/ `RevokeApiKey` 會讓最後一個有效 admin（ACTIVE、有 admin role、至少一把可用 key 的 account）失去資格；訊息固定。config `auth.api_keys` 的 static key 不算 |
| `account.disabled` | `FAILED_PRECONDITION` | identity domain | ResolveAPIKey：key 沒事但 account 是 DISABLED（rpc 看到的是 `auth.invalid_credential`） |
| `account.not_found` | `NOT_FOUND` | identity domain | 含 `CreateApiKey` / `ListApiKeys` / `ListAccountIdentities` 指到不存在的 account |
| `account.duplicate` | `ALREADY_EXISTS` | identity domain | 同 id 再 Add（repo 層；id 是 UUID v7，舊資料為 v4，實際上不會發生） |
| `api_key.invalid_id` / `api_key.invalid_account_id` / `api_key.invalid_name` / `api_key.invalid_secret` | `INVALID_ARGUMENT` | identity domain | aggregate 不變量；`invalid_secret` 也是 ResolveAPIKey 對太短的 key 的答案 |
| `api_key.invalid_expiry` | `INVALID_ARGUMENT` | identity domain | `CreateApiKey.expire_time` 不在未來 |
| `api_key.already_revoked` | `FAILED_PRECONDITION` | identity domain | Revoke 已 REVOKED 的 key |
| `api_key.revoked` / `api_key.expired` | `FAILED_PRECONDITION` | identity domain | ResolveAPIKey：`Usable(now)` 拒絕（rpc 看到的是 `auth.invalid_credential`） |
| `api_key.not_found` | `NOT_FOUND` | identity domain | `RevokeApiKey` 的 id 不存在；ResolveAPIKey 的 hash 對不上 |
| `api_key.duplicate` | `ALREADY_EXISTS` | identity domain | 同 id 或同 hash 再 Add（repo 層） |
| `identity.invalid_issuer` / `identity.invalid_subject` / `identity.invalid_binding_account` / `identity.invalid_mapping` | `INVALID_ARGUMENT` | identity domain（`externalidentity`） | external identity 不變量：issuer / subject / account 不能空、`mapping` 只認 `bound` 與 `auto_provision` |
| `identity.unbound` | `NOT_FOUND` | identity domain（`externalidentity`） | `(iss, sub)` 沒有綁定任何 account（`mapping: bound`）。resolver 把它交給呼叫端（#272：未綁定只能 `ClaimInvite`）；不進 cache |
| `identity.already_bound` | `ALREADY_EXISTS` | identity domain（`externalidentity`） | `(iss, sub)` 已綁定：`ClaimInvite` 的 sub 已有 account，或 `auto_provision` 同一身分並發（輸家整筆 rollback，resolver 重讀） |
| `invite.invalid_id` / `invite.invalid_email` / `invite.invalid_account` | `INVALID_ARGUMENT` | identity domain（`invite`） | invite 不變量；`invalid_email` 也是 `CreateInvite` / `worker bootstrap --admin-email` 給了不像 email 的字串 |
| `invite.not_found` | `NOT_FOUND` | identity domain（`invite`） | `ClaimInvite`：這個 email 沒有任何 invite |
| `invite.expired` / `invite.already_claimed` / `invite.email_unverified` / `invite.email_mismatch` | `FAILED_PRECONDITION` | identity domain（`invite`） | `ClaimInvite` 拒絕：超過 7 天、已被用過（含兩個 sub 搶同一張，輸家拿這個）、token 的 `email_verified` 不是 true、email 與邀請不符。被拒的 claim 不會花掉 invite |
| `account.session_revoked` / `account.missing_iat` | `PERMISSION_DENIED` | identity domain（`account.CheckSession`，經 external resolver） | token 的 `iat` 早於 `sessions_revoked_at`，或 token 沒有 `iat`（無從比較，一律拒絕） |
| `message.duplicate` | （不到 rpc） | `usecasex.Idempotent` | consumer inbox 看到重送的 message id：handler 回它、broker ack、不 retry；rpc path 沒 message id 不會發 |
| `account.invalid_status` / `api_key.invalid_status` | `INTERNAL`（header 不帶 code） | domain | 從 DB 讀回來的狀態 / 數量不合法：資料壞了，不是 client 的錯 |

Kind → Connect code 只在 `server.ToConnectError` 一處；consumer 不看 code：`kernel.Error` 一律 drop（進 poison），其他 error retry。
