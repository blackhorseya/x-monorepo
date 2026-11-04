# Order history（訂單事件歷程）

每張訂單一份**只追加、不修改**的狀態轉換紀錄：誰、何時、從哪個狀態到哪個狀態、為什麼。表是 `ordering.order_history`
（migration `0010`）。#427 切片 1 只有寫入路徑；`GetOrderHistory` RPC 與 Console 是切片 2、3。

## 為什麼不直接讀 outbox

`ordering.events_v1_*` 是傳遞管道：`worker prune` 會刪掉每個 group 都 ack 過的列、payload 是 proto bytes、沒有 actor。
history 是紀錄，所以另開一張表，**`worker prune` 不碰它**（有測試從真入口 `worker prune` 驗證）。

## 寫入路徑（結構性）

`ordering.ProvideEventPublisher` 把 outbox publisher 包進 `adapters/events.Recorder`，所有 use case 都經過它 publish。
Recorder 對每個 event 先 publish、再 `HistoryStore.Append`，兩者在 use case 的同一個 transaction 裡（`decorateTx`，
或 `CreateOrder` 自己開的 `Transactor`），所以一起 commit、一起 rollback。新增的 use case 只要 publish 就會被記，不用記得呼叫 record。

不發 event 的路徑不寫 history：冪等重放、`ErrIdempotencyConflict`（在 Publish 之前就 return）、被 domain 拒絕或 no-op 的 payment event。

## 每一列的欄位

| 欄位 | 來源 |
|---|---|
| `order_id`, `seq` | `seq` 是每張訂單 1..N，由 store 寫入時指派 |
| `event` | CREATED／CONFIRMED／PAID／COMPLETED／CANCELLED，由 `to_status` 決定（PENDING = CREATED） |
| `from_status`, `to_status` | domain event 的 `From()`／`To()`；CREATED 的 `from_status` 是 NULL |
| `occurred_at` | `e.OccurredAt()`（domain clock），不是 DB 的 `now()` |
| `actor_kind`, `actor_subject` | 見下 |
| `cause` | domain event 的 `Cause()`，固定列舉 |

### seq

`INSERT … SELECT COALESCE(MAX(seq),0)+1 … WHERE order_id=$1`，**在該訂單 `Save` 之後**、同一個 transaction 讀。
`Save` 的 UPDATE 會拿 `orders` 的 row lock 直到 commit，同一張單的兩個 writer 因此被序列化，後到的在 version 比對時就 stale、
重新載入後才會再寫。`seq` 不等於 `version`：`ApplyPaymentCaptured` 在一次 `Save` 裡產生 PAID 和 COMPLETED 兩列。
primary key `(order_id, seq)` 是最後防線：沒先 `Save` 的 writer 撞 key 會得到 infra error（不吞、不轉成 domain error、不重試）。

### actor

由 `Recorder` 依 transition 的 cause 與 `contextx.Principal` 決定：

- cause 是 `RPC_REQUEST`（rpc 的 Create／Cancel／Complete）：有 principal 時依 `auth.admin_role` 分 `ADMIN`／`CUSTOMER`，
  `actor_subject` 是 `Principal.Subject`（account id）；沒有 principal（`auth.mode=off`）是 `ANONYMOUS`。
- 其他 cause（consumer 路徑）：一律 `SYSTEM`，`actor_subject` 空。

`actor_subject` 只能給 admin 看（切片 2 的 RPC 負責遮蔽）。

### cause（固定列舉）

`RPC_REQUEST`、`PAYMENT_CAPTURED`、`PAYMENT_EXPIRED`、`STOCK_RESERVED`、`CANCELLATION_REQUESTED`、`UNSPECIFIED`。
cause 跟著 domain event 走：domain 自己知道的在 domain 決定（Confirm = `STOCK_RESERVED`，`ApplyPayment*` 各自的 cause，
New／Complete = `RPC_REQUEST`）；Cancel 的來源 domain 不知道，由 adapter 在 `dto.CancelOrderInput.Source` 明說
（rpc = `RPC_REQUEST`、consumer = `CANCELLATION_REQUESTED`），沒說（零值）就被 `order.ErrInvalidCause` 拒絕，不會悄悄變成別的值。

`OrderCancellationRequested.reason` 是 inventory 自己組的字串（含 SKU 與時長），不可信也不穩定：**不寫進 history，也不寫進 log**。
切片 2 才會在該 event 加 typed 的 `cause` 欄位。

## 既有訂單不 backfill

migration 之前建立的訂單沒有 history，或只有 migration 之後的轉換（缺前段）。`HistoryStore.List` 對沒有任何列的訂單回空清單、不是錯誤。

## migration

只有 `CREATE TABLE`，**不加指向 `ordering.orders` 的 FK**：加 FK 要在 `orders` 上拿鎖，而整份 migration 在同一個 transaction 裡，
鎖會持有到 commit（#318）。要加另開一題，先量測鎖的影響。
