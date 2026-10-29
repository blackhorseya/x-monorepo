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
| `auth.invalid_credential` | `UNAUTHENTICATED` | auth interceptor | 帶了但對不上（未知 API key、JWT 簽章 / exp / iss / aud 不對）。identity 的 key 被 revoke / 過期 / account disabled、或 resolver 查 DB 失敗，client 看到的也是這個（不洩漏原因；resolver 失敗另有 error log） |
| `auth.permission_denied` | `PERMISSION_DENIED` | `server.RequireRole` | 有身分但缺角色（AdminService、`InventoryService.ExpireReservations`、`IdentityService` 除 `WhoAmI` 外都要 `auth.admin_role`） |
| `concurrent_update` | `ABORTED` | `server.ToConnectError` | optimistic lock 撞版（`order.ErrStale` / `stock.ErrStale` / `account.ErrStale` / `apikey.ErrStale`，wrap `kernel.ErrConcurrentUpdate`）；Order 的 cancel / confirm / complete 已先在 use case 內重試 3 次，仍撞才會出現；重送即可，刻意沒有前綴也不是 `kernel.Error` |
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
| `stock.invalid_sku` / `stock.negative_quantity` / `stock.invalid_quantity` | `INVALID_ARGUMENT` | inventory domain | SetStock / Reserve 的參數 |
| `stock.below_reserved` | `FAILED_PRECONDITION` | inventory domain | SetStock 想把 on-hand 壓到低於已 reserved |
| `stock.release_exceeds_reserved` | `FAILED_PRECONDITION` | inventory domain | Release 超過 reserved |
| `stock.commit_exceeds_reserved` | `FAILED_PRECONDITION` | inventory domain | Commit 超過 reserved（stock 列與 reservation 不一致：資料壞了） |
| `stock.not_found` | `NOT_FOUND` | inventory domain | |
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
| `account.not_found` | `NOT_FOUND` | identity domain | 含 `CreateApiKey` / `ListApiKeys` 指到不存在的 account |
| `account.duplicate` | `ALREADY_EXISTS` | identity domain | 同 id 再 Add（repo 層；id 是 UUID，實際上不會發生） |
| `api_key.invalid_id` / `api_key.invalid_account_id` / `api_key.invalid_name` / `api_key.invalid_secret` | `INVALID_ARGUMENT` | identity domain | aggregate 不變量；`invalid_secret` 也是 ResolveAPIKey 對太短的 key 的答案 |
| `api_key.invalid_expiry` | `INVALID_ARGUMENT` | identity domain | `CreateApiKey.expire_time` 不在未來 |
| `api_key.already_revoked` | `FAILED_PRECONDITION` | identity domain | Revoke 已 REVOKED 的 key |
| `api_key.revoked` / `api_key.expired` | `FAILED_PRECONDITION` | identity domain | ResolveAPIKey：`Usable(now)` 拒絕（rpc 看到的是 `auth.invalid_credential`） |
| `api_key.not_found` | `NOT_FOUND` | identity domain | `RevokeApiKey` 的 id 不存在；ResolveAPIKey 的 hash 對不上 |
| `api_key.duplicate` | `ALREADY_EXISTS` | identity domain | 同 id 或同 hash 再 Add（repo 層） |
| `message.duplicate` | （不到 rpc） | `usecasex.Idempotent` | consumer inbox 看到重送的 message id：handler 回它、broker ack、不 retry；rpc path 沒 message id 不會發 |
| `order.invalid_status` / `reservation.invalid_status` / `stock.corrupt_quantities` / `account.invalid_status` / `api_key.invalid_status` | `INTERNAL`（header 不帶 code） | domain | 從 DB 讀回來的狀態 / 數量不合法：資料壞了，不是 client 的錯 |

Kind → Connect code 只在 `server.ToConnectError` 一處；consumer 不看 code：`kernel.Error` 一律 drop（進 poison），其他 error retry。
