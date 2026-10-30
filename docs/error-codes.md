# Error codes

<sub>← [README](../README.md)</sub>

## Error codes：`x-error-code` 總表

每個失敗的 RPC 都帶 Connect code（HTTP status 由它決定）+ header `x-error-code`，client 判斷用後者（`console` 的 `describe()` 就是印它）。
來源三種：**platform interceptor**（`request.*` / `auth.*`）、**admin**（`admin.*`，`pkgs/platform/admin` 自己設）、**domain error**
（`kernel.NewError(kind, code, msg)`，`server.ToConnectError` 依 `Kind` 映射：InvalidArgument → `INVALID_ARGUMENT`、NotFound → `NOT_FOUND`、
FailedPrecondition → `FAILED_PRECONDITION`、Conflict → `ALREADY_EXISTS`、PermissionDenied → `PERMISSION_DENIED`；其他 → `INTERNAL`，**沒有** `x-error-code`，訊息固定 `internal error`）。
加新的 domain error 就在該 aggregate 的 `errors.go` 加一行，前綴 = aggregate 名（跨 aggregate 的 BC 層規則用 BC 名，如 `identity.last_admin`）；這張表是手寫的，改了記得同步。

| `x-error-code` | Connect code | 誰設 | 什麼時候 |
|---|---|---|---|
| `request.validation_failed` | `INVALID_ARGUMENT` | validate interceptor | request 違反 proto 的 `buf.validate` 規則；`Violations` detail 列欄位（rule id 如 `admin.poison_offsets_xor_all` 也在裡面） |
| `request.timeout` | `DEADLINE_EXCEEDED` | timeout interceptor | 超過 `http.timeout`，不管底下是什麼錯 |
| `request.rate_limited` | `RESOURCE_EXHAUSTED` | rate limit interceptor | 超過 `http.rate_limit`，另帶 `Retry-After` |
| `auth.unauthenticated` | `UNAUTHENTICATED` | auth interceptor | `auth.mode=required` 且沒帶 credential |
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了但對不上（未知 API key、JWT 簽章 / exp / iss / aud 不對）。identity 的 key 被 revoke / 過期 / account disabled、或 resolver 查 DB 失敗，client 看到的也是這個（不洩漏原因；resolver 失敗另有 error log）。OIDC token（`auth.oidc`）同理：簽章 / `iss` / `aud` / `exp` / 缺 `iat` 不對、對應的 account 已停用、`iat` 早於 `sessions_revoked_at`、`(iss, sub)` 查 DB 失敗，一律這個碼加固定訊息（token 本身或帳號被拒：`token could not be verified`；resolver 查不到答案：`credential could not be verified`，原因只進 server log，且 log 不含 token）。逾時（`auth.resolver_timeout`）後才回來的答案一律拒絕，連「未綁定」也不例外，不會放行 `ClaimInvite`。帶了 `X-API-Key` 時不看 Bearer，key 錯就是這個，不會 fallback |
| `auth.unbound_identity` | `PERMISSION_DENIED` | auth interceptor | OIDC token 驗過了，但 `(iss, sub)` 沒綁定任何 account（`mapping: bound`），而這個 procedure 不是 `IdentityService.ClaimInvite`。未綁定的身分只能呼叫 `ClaimInvite`（`auth.mode=optional` 也一樣，不會變 anonymous）。要先請 admin 發邀請，再用 `ClaimInvite` 綁定 |
| `auth.permission_denied` | `PERMISSION_DENIED` | `server.RequireRole` | 有身分但缺角色（AdminService、`InventoryService.SetStock` 與 `ExpireReservations`、`CatalogService.UpsertProduct` 與 `DeactivateProduct`、`IdentityService` 除 `WhoAmI` 與 `ClaimInvite` 外都要 `auth.admin_role`） |
| `concurrent_update` | `ABORTED` | `server.ToConnectError` | optimistic lock 撞版（`order.ErrStale` / `stock.ErrStale` / `product.ErrStale` / `account.ErrStale` / `apikey.ErrStale` / `invite.ErrStale` / `reservation.ErrStale`，wrap `kernel.ErrConcurrentUpdate`）；Order 的 cancel / confirm / complete 已先在 use case 內重試 3 次，仍撞才會出現；重送即可，刻意沒有前綴也不是 `kernel.Error` |
| `pagination.invalid_page_size` | `INVALID_ARGUMENT` | `platform/pagination` | `page_size` 負數 |
| `pagination.invalid_token` | `INVALID_ARGUMENT` | `platform/pagination` | `page_token` 解不開，或帶的 filter 跟這次 request 不同（ListOrders / ListStocks 把 filter 編進 token） |
| `admin.no_database` | `FAILED_PRECONDITION` | admin | server 沒接 Postgres（`postgres.url` 空）：poison 表與 outbox / inbox 都在 DB |
| `admin.unknown_bounded_context` | `INVALID_ARGUMENT` | admin | `bounded_context` 不在 `apps/rpc` 列的 schema 清單 |
| `admin.poison_not_found` | `NOT_FOUND` | admin | `GetPoison` 的 offset 不存在 |
| `order.permission_denied` | `PERMISSION_DENIED` | ordering application（`ownership.Policy`） | 非 admin 的 principal 替別人下單（`CreateOrder`）或列別人的訂單（`ListOrders`）；admin role = `auth.admin_role`。讀、取消、完成別人的訂單（`GetOrder` / `CancelOrder` / `CompleteOrder`）是 `order.not_found`，不是這個 |
| `order.invalid_id` / `order.empty_customer` / `order.no_items` / `order.invalid_sku` / `order.invalid_quantity` | `INVALID_ARGUMENT` | ordering domain | aggregate 不變量（consumer path 沒有 protovalidate，所以留著） |
| `order.invalid_status_filter` / `order.invalid_cursor` | `INVALID_ARGUMENT` | ordering domain | ListOrders 的 filter / cursor 壞掉 |
| `order.cannot_cancel` | `FAILED_PRECONDITION` | ordering domain | 不是 PENDING / CONFIRMED 的 order 被 Cancel（COMPLETED 是終點） |
| `order.cannot_confirm` | `FAILED_PRECONDITION` | ordering domain | 不是 PENDING 的 order 收到 `StockReserved`（consumer path：drop） |
| `order.cannot_complete` | `FAILED_PRECONDITION` | ordering domain | 不是 CONFIRMED 的 order 被 Complete（PENDING 還沒有庫存可 commit；再按一次也是這個） |
| `order.not_found` | `NOT_FOUND` | ordering domain / application | 沒有這筆；非 admin 的 principal 讀、取消、完成別人的訂單也是它（`GetOrder` / `CancelOrder` / `CompleteOrder`，不洩漏存在與否） |
| `order.duplicate` | `ALREADY_EXISTS` | ordering domain | 同 id 再 Save（repo 層） |
| `order.unknown_sku` / `order.inactive_sku` | `INVALID_ARGUMENT` | ordering domain（`CreateOrder`） | catalog 報不出價：SKU 沒有 product（`unknown_sku`，多半打錯）／product 已下架（`inactive_sku`）。**整張單被拒、沒寫入、沒發 event、冪等 key 也沒被花掉**（商品上架後同一個 key 重送就成立）。訊息固定，不回是哪個 SKU（呼叫端知道自己送了什麼） |
| `order.mixed_currency` | `INVALID_ARGUMENT` | ordering domain（`CreateOrder`） | 同一張單的行落在兩種幣別（每個 product 各自的幣別，一張單只能一種）；沒寫入 |
| `order.idempotency_key_reused` | `ALREADY_EXISTS` | ordering domain（`CreateOrder`） | 同一個 customer 的 `idempotency_key` 已用在**內容不同**的請求上（items 的 sku / quantity / **順序**任一不同；價格不是請求內容，是 catalog 的）；沒寫入、沒發 event。訊息固定，不帶訂單、items、key；要換 key 才能下新單。內容相同的重送不是錯誤，回原訂單 |
| `order.idempotency_conflict` | （不到 rpc） | ordering repo（`Save`） | 新單的 (customer, key) 被別人先佔了：use case 接住、回讀勝出的那筆再決定回放或 `order.idempotency_key_reused`；`INTERNAL` Kind 只是保險，正常不會到 `ToConnectError` |
| `stock.invalid_sku` / `stock.negative_quantity` / `stock.invalid_quantity` | `INVALID_ARGUMENT` | inventory domain | SetStock / Reserve 的參數；`stock.invalid_sku` = SKU 空或超過 64 **bytes**（與 catalog 同規則；超過 64 字元的請求先被 proto 擋成 `request.validation_failed`，64 字元以內但超過 64 bytes 的多位元組 SKU 才走這個 code） |
| `stock.below_reserved` | `FAILED_PRECONDITION` | inventory domain | SetStock 想把 on-hand 壓到低於已 reserved |
| `stock.release_exceeds_reserved` | `FAILED_PRECONDITION` | inventory domain | Release 超過 reserved |
| `stock.commit_exceeds_reserved` | `FAILED_PRECONDITION` | inventory domain | Commit 超過 reserved（stock 列與 reservation 不一致：資料壞了） |
| `stock.not_found` | `NOT_FOUND` | inventory domain | |
| `product.invalid_sku` / `product.invalid_name` / `product.invalid_price` | `INVALID_ARGUMENT` | catalog domain | UpsertProduct / GetProduct / DeactivateProduct / 批次報價的參數：sku 1–64 bytes、name 1–200 字元且不能全是空白（`strings.TrimSpace` 的定義）、price 必須是非負金額加 3 個大寫字母的幣別——domain 對 `Money` 重新檢查（`Multiply(-1)`、`ZeroMoney("usd")` 這類繞過 `NewMoney` 的值也擋）（rpc 先被 protovalidate 擋，這是直呼 use case 的那條） |
| `product.unsupported_currency` | `INVALID_ARGUMENT` | catalog domain | 幣別格式對但不是已指派的 ISO 4217 代碼（`AAA`、`ZZZ`…）；白名單是 SIX List One（`domain/product/currency_codes.go`，與 `catalog.currencies` 同一份）。protovalidate 只擋格式，所以這個一定走到 domain |
| `money.invalid_amount` / `money.invalid_currency` | `INVALID_ARGUMENT` | kernel `Money` | 金額為負、幣別不是 3 個大寫英文字母（ISO 4217 形狀）；`UpsertProduct` 的價格（rpc 先被 protovalidate 擋） |
| `product.not_found` | `NOT_FOUND` | catalog domain | `GetProduct` / `DeactivateProduct` 的 SKU 沒有 product（`UpsertProduct` 會建立它，不會回這個） |
| `reservation.invalid_order_id` / `reservation.no_lines` / `reservation.invalid_sku` / `reservation.invalid_quantity` | `INVALID_ARGUMENT` | inventory domain | aggregate 不變量 |
| `reservation.invalid_status_filter` / `reservation.invalid_cursor` | `INVALID_ARGUMENT` | inventory domain | ListReservations 的 filter / cursor |
| `reservation.too_many_order_ids` | `INVALID_ARGUMENT` | inventory domain | `BatchGetReservations` 一次超過 100 個 order id（rpc 先被 protovalidate 擋，這是直呼 use case 的那條）；空清單與空字串 id 走 `reservation.invalid_order_id` |
| `reservation.invalid_older_than` | `INVALID_ARGUMENT` | inventory domain | ExpireReservations 的 `older_than` 不是正數（rpc 先被 protovalidate 擋，這是 CLI / 直呼 use case 的那條） |
| `reservation.not_reserved` | `FAILED_PRECONDITION` | inventory domain | 只有 RESERVED 能 Release / Commit / Expire（重送的 `OrderCancelled` / `OrderCompleted`、過期後才到的 `OrderCancelled`：drop） |
| `reservation.stock_missing` | `FAILED_PRECONDITION` | inventory domain | Release / Commit / Expire 時某 SKU 的 stock 列不在了 |
| `reservation.not_found` | `NOT_FOUND` | inventory domain | `GetReservation`：worker 還沒處理這張 order（console 顯示 `none yet`） |
| `reservation.duplicate` | `ALREADY_EXISTS` | inventory domain | 同一 order 第二張 reservation（repo `Add`）；reserve / release usecase 把它當「已處理」吞掉，rpc 實際上看不到 |
| `account.invalid_id` / `account.invalid_name` / `account.invalid_role` | `INVALID_ARGUMENT` | identity domain | aggregate 不變量（role 不能是空字串；protovalidate 先擋 rpc 那條） |
| `account.invalid_cursor` | `INVALID_ARGUMENT` | identity domain | ListAccounts 的 page token 解得開但沒有 id |
| `account.already_disabled` / `account.already_active` | `FAILED_PRECONDITION` | identity domain | Disable 已 DISABLED / Enable 已 ACTIVE 的 account |
| `identity.last_admin` | `FAILED_PRECONDITION` | identity domain（`account.GuardLastAdmin` / `GuardLastAdminKey`） | `DisableAccount` / `AssignRoles`（拿掉 `auth.admin_role`）/ `RevokeApiKey` 會讓最後一個有效 admin（ACTIVE、有 admin role、至少一把可用 key 的 account）失去資格；訊息固定。config `auth.api_keys` 的 static key 不算 |
| `account.disabled` | `FAILED_PRECONDITION` | identity domain | ResolveAPIKey：key 沒事但 account 是 DISABLED（rpc 看到的是 `auth.invalid_credential`） |
| `account.not_found` | `NOT_FOUND` | identity domain | 含 `CreateApiKey` / `ListApiKeys` / `ListAccountIdentities` 指到不存在的 account |
| `account.duplicate` | `ALREADY_EXISTS` | identity domain | 同 id 再 Add（repo 層；id 是 UUID，實際上不會發生） |
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
| `order.invalid_status` / `reservation.invalid_status` / `stock.corrupt_quantities` / `account.invalid_status` / `api_key.invalid_status` | `INTERNAL`（header 不帶 code） | domain | 從 DB 讀回來的狀態 / 數量不合法：資料壞了，不是 client 的錯 |

Kind → Connect code 只在 `server.ToConnectError` 一處；consumer 不看 code：`kernel.Error` 一律 drop（進 poison），其他 error retry。
