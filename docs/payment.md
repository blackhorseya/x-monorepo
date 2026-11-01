# Payment

<sub>← [README](../README.md)</sub>

> **狀態：設計稿（#367 切片 0）。以下全部尚未實作**——`pkgs/payment`、`apps/payment-rpc`、`PAID`、`OrderConfirmed.total`、`ordering.require_payment` 目前在 repo 裡都不存在。
> 「保證」欄只列已指定到切片測試的條目；測試名稱是預定名稱，實作時以實際為準，寫不出測試的就不在這裡當保證。
> 七個設計決定由 Sean 逐題確認（#367），改動任何一項都要回 Sean。標 **〔補充〕** 的是 issue 沒展開、由本文補上的細節，**審閱時請一併確認**；「未決事項」一節列出本文無法自行決定的地方。

`pkgs/payment` 是第五個 bounded context：補上訂單從 `CONFIRMED` 到完成之間的**收款**。
流程是 建單 → 預留 → `CONFIRMED` → 付款 → `PAID` → `COMPLETED`；`CONFIRMED` 的語意（庫存已保留）不變。
付款細節只存在 Payment，訂單**不加** `payment_status` 欄位。卡片資料完全不進系統（hosted checkout）。

## 1. 形狀

```
顧客 ─► apps/rpc ──(service key + customer_id)──► apps/payment-rpc serve ──► provider（第一版 mock）
                                                      ▲   │ 自己的 payment schema
provider ──webhook（簽章）─────────────────────────────┘   │ outbox
                                                          ▼
ordering ──OrderConfirmed / OrderCancelled──► payment-rpc consume ──PaymentCaptured / PaymentExpired / PaymentRefunded──► ordering
payment-rpc expire（CronJob）──► provider 查狀態 ──► PaymentExpired
```

- `apps/payment-rpc` 是新 binary，三個子命令：`serve`（RPC 與 webhook）、`consume`（訂閱 `OrderConfirmed`、`OrderCancelled`，處理 webhook 事件）、`expire`（CronJob）。自己的 `payment` schema，provider 密鑰只有它持有。
- `apps/worker` **不載入** Payment。`apps/rpc` 只依賴 payment 的 `contract/` 與 `adapters/client`，不載入領域程式（同 catalog 的 #320 作法；用 archtest 固定）。
- `payment-rpc` 掛掉時，只有付款操作回 503，下單與預留不受影響。
- **剩下的耦合**：Postgres broker 下，Payment 要讀 ordering 的 outbox 表（與 ordering 讀 inventory 的 outbox 同一種耦合）。

### 與 `CLAUDE.md` 規則的關係

| 規則（`CLAUDE.md`） | 本設計 | 性質 |
|---|---|---|
| 「新增 Bounded Context」清單：`apps/worker` 的 `NewWorker` 收該 BC 的 `Subscriptions`、`poison.go` 的 `schemas` 加它 | Payment 的 `Subscriptions` 掛在 `payment-rpc consume` 自己的 router，**不進 `apps/worker`** | **例外**，只限 payment；不改規則。原因：provider 密鑰只有 payment 持有，且 worker 載入 payment 會讓 worker 也持有 provider 憑證。poison / prune 怎麼處理見「未決事項」U3 |
| `apps/rpc` 不載入 catalog（#320） | 同樣不載入 payment，經 `contract/` 的 Connect client 問 `payment-rpc` | 沿用既有規則 |
| 「decorator 只在 `provider.go` 加」「usecase 不自己開 transaction」 | `StartPayment` 要呼叫 provider，不在 DB transaction 內：走 `decorateNoTx()`，用法比照 `CreateOrder`（持有 `port.Transactor`，只包住寫入） | **第二個例外**，與 #320 同類；寫入前先打外部服務的 usecase。範圍：`StartPayment`、`expire` 的 provider 查詢、退款呼叫 |
| platform `server.New` 的 `routes` 只收 Connect `Route` | webhook 是普通 HTTP route，要在 platform 開一個掛 gin route 的 option（切片 2 或 3 一併做） | **platform 變更**，不是規則例外；auth interceptor 只包 Connect，gin route 永遠不經它 |
| 「Auth 只做 authentication」 | 不變。擁有權比對（`customer_id`）在 Payment 的 application 層，不在 interceptor | 沿用 |

## 2. 訂單端的變更（切片 1）

- 新狀態 `PAID`：`CONFIRMED → PAID → COMPLETED`。`PAID` = 收到款；`COMPLETED` = 交易結束、庫存售出，語意不變。
- **第一版 `PAID` 由付款成功自動走到 `COMPLETED`** 〔補充：ordering 處理 `PaymentCaptured` 時，在同一個 transaction 內先 `MarkPaid` 再 `Complete`，發 `OrderPaid` 與 `OrderCompleted` 兩個事件；inventory 沿用 `OrderCompleted` 去 commit 預留〕。
- `PAID` 之後不能走 `CancelOrder`（`CancelOrder` 對 `PAID` 沿用現有的 `order.cannot_cancel`（`FAILED_PRECONDITION`，`pkgs/ordering/domain/order/errors.go`），做法是 `CanTransitionTo` 不放行 `PAID → CANCELLED`）。訂單一旦 `CANCELLED` 就不復活。
- `OrderConfirmed` 新增 `total`（`common.v1.Money`）。**舊事件沒有 `total` 時視為明確錯誤，不能當成 0 元收款**：payment 的 consumer 回 domain error（drop，不 retry）並告警，不建付款單。
- **已知限制**：inventory 目前在 `OrderCompleted` 才 commit 預留。第一版 `PAID` 立即走到 `COMPLETED`，沒有問題。之後 Fulfillment 讓 `PAID` 停留時，要改成 `OrderPaid` 時 commit，否則已付款訂單的預留可能過期（24 小時 TTL 的 `expire` 會把它還掉）。
- Console 顯示 `PAID`（切片 1）。

## 3. 付款單（`payment.Payment`）

| 欄位 | 說明 |
|---|---|
| `id` | 付款單 ID（UUID v7，#365）；**退款呼叫的冪等 key** |
| `order_id` | 一張訂單最多一張付款單（`unique(order_id)`）〔補充〕 |
| `customer_id` | 取自 `OrderConfirmed`，擁有權比對用 |
| `amount`（`kernel.Money`）| 取自 `OrderConfirmed.total` |
| `status` | 見下 |
| `expires_at` | 建單當下 + 窗口，預設 30 分鐘，由 Payment 管理 |
| `provider` / `provider_ref` / `checkout_url` / `checkout_expires_at` | `StartPayment` 才有值 |
| `order_cancelled` | 〔補充〕Payment 已收到 `OrderCancelled`。晚到的收款靠它判斷要退款 |
| `version` | optimistic lock，同其他 BC |

### 狀態機

| 狀態 | 意義 | 終態？ |
|---|---|---|
| `PENDING` | 已建、計時中；未收款 | 否 |
| `CAPTURED` | provider 確認收款，金額與幣別相符 | 否（之後可能 `REFUNDING`） |
| `EXPIRED` | 窗口到期且 provider 查無收款 | 否（晚到的收款仍可進 `CAPTURED`） |
| `CANCELLED` 〔補充，issue 沒列〕 | 訂單被取消（`OrderCancelled`），Payment 取消 provider 端付款 | 否（同 `EXPIRED`） |
| `REFUNDING` | 要全額退款；provider 退款呼叫進行中或待重試 | 否 |
| `REFUNDED` | 退款完成 | 是 |
| `NEEDS_REVIEW` 〔補充，issue 沒列〕 | 收到款但金額／幣別與付款單不符；不進 `CAPTURED`，人工處理並告警 | 是（系統不再動它） |

**為何補 `CANCELLED`**：「取消路徑」需要一個 Payment 自己的結果；若重用 `EXPIRED`，「訂單先取消」與「逾時」的告警、指標與退款判斷會混在一起。
**為何補 `NEEDS_REVIEW`**：issue §5.4 要求不符時「不進 `CAPTURED`、標記人工處理並告警」，需要一個可查詢、可被 metrics 數到的落點；留在 `PENDING` 會被 `expire` 當成沒付款而過期，錢已經在 provider 那邊。
**沒有 `FAILED`**：hosted checkout 讓顧客在窗口內自行重試，provider 的單次失敗事件不改付款單狀態，窗口到期才 `EXPIRED`。（此為第一版取捨，要加 `FAILED` 需回 Sean。）

### 轉移與觸發來源

| # | 轉移 | 觸發來源 | 條件／動作 | 發布事件 |
|---|---|---|---|---|
| T1 | ∅ → `PENDING` | `consume`：`OrderConfirmed` | 驗 `total` 存在；`unique(order_id)` 重送忽略；不打 provider | — |
| T2 | `PENDING` →（不變，補 `provider_ref`、`checkout_url`）| `serve`：`StartPayment` | 顧客本人；未過期；向 provider 開 hosted checkout。**重複呼叫回傳尚未過期的同一個 URL**；`checkout_expires_at` 已過則重開 | — |
| T3 | `PENDING` → `CAPTURED` | `consume`：webhook 事件（`payment.captured`） | 金額、幣別與付款單相符 | `PaymentCaptured` |
| T4 | `PENDING` → `NEEDS_REVIEW` | 同 T3 | 金額或幣別不符 | — （告警） |
| T5 | `PENDING` → `EXPIRED` | `expire`（CronJob） | `now ≥ expires_at`，且**先向 provider 查一次狀態**確認沒收款 | `PaymentExpired` |
| T6 | `PENDING` → `CAPTURED` | `expire` | 同 T5 但 provider 查到已收款（webhook 沒送到），金額相符 | `PaymentCaptured` |
| T7 | `PENDING` → `CANCELLED` | `consume`：`OrderCancelled` | 取消 provider 端 checkout（best-effort，失敗不阻擋）；設 `order_cancelled` | — |
| T8 | `EXPIRED` / `CANCELLED` → `CAPTURED` | webhook（逾時或取消後 provider 才通知成功） | 錢已動：必須記錄；金額相符才進 | `PaymentCaptured` |
| T9 | `EXPIRED` / `CANCELLED` → `NEEDS_REVIEW` | webhook | 同 T8 但金額／幣別不符 | — （告警） |
| T10 | `CAPTURED` → `REFUNDING` | `consume`：`OrderCancelled`，或 T3/T6/T8 進入 `CAPTURED` 時 `order_cancelled` 已為真 | 兩個順序（先收款後取消／先取消後收款）收斂到同一個條件：`CAPTURED ∧ order_cancelled`。`PAID` 的訂單不會發 `OrderCancelled`，所以不會誤退 | `PaymentRefundRequested`（內部） |
| T11 | `REFUNDING` → `REFUNDED` | `consume`：處理 `PaymentRefundRequested` | 呼叫 provider 退款（冪等 key = 付款單 `id`）成功。失敗有界重試，超過次數進 poison 並告警，狀態維持 `REFUNDING` | `PaymentRefunded`（目前沒有 consumer） |
| — | 其餘組合 | — | 晚到的舊事件忽略（「只往前走」）；不在上表的轉移一律拒絕 | — |

`order_cancelled` 在 T7、T8、T9、T10 的觸發事件處理時一併寫入；`OrderCancelled` 比 `OrderConfirmed` 先到（不同 topic 無順序保證）時的處理見 §4「亂序」。

## 4. 事件清單

Topic 名稱 = proto 全名（同現有慣例）。下表「欄位」只列要點，完整定義在切片 1／2 的 proto。

| 事件 | 發布者 | 訂閱者 | 欄位 | 說明 |
|---|---|---|---|---|
| `ordering.v1.OrderConfirmed`（改） | ordering | **payment**（新） | `order_id`、`customer_id`、`confirmed_at`、**`total`（新）** | T1 |
| `ordering.v1.OrderPaid`（新） | ordering | 目前沒有 | `order_id`、`customer_id`、`paid_at` | 為 Fulfillment 預留（見 §2 已知限制） |
| `ordering.v1.OrderCancelled`（現有） | ordering | inventory、**payment**（新） | `order_id`、`customer_id`、`cancelled_at` | T7、T10 |
| `ordering.v1.OrderCompleted`（現有） | ordering | inventory | 不變 | payment **不**訂閱——見未決事項 U1 |
| `payment.v1.PaymentCaptured` | payment | ordering | `payment_id`、`order_id`、`customer_id`、`amount`、`captured_at` | ordering 決定 `PAID`／拒絕 |
| `payment.v1.PaymentExpired` | payment | ordering | `payment_id`、`order_id`、`customer_id`、`expired_at` | ordering 取消訂單；inventory 走現有 `OrderCancelled` 還庫存 |
| `payment.v1.PaymentRefunded` | payment | 無 | `payment_id`、`order_id`、`amount`、`refunded_at` | 保留給之後 |
| `payment.v1.PaymentRefundRequested`（內部） | payment | payment（`consume`） | `payment_id` | 讓退款呼叫在 DB transaction 之外、走 eventbus 的 retry／poison；不對外承諾 |

`ordering.v1.OrderCancellationRequested` 的註解寫著「例如 payment timeout」，但本設計**不用它**：逾時一律走 `PaymentExpired`，由 ordering 的狀態機判定（切片 3 順手改註解）。

### 亂序與重送

| 情境 | 處理 |
|---|---|
| 同一事件重送 | consumer inbox（`payment.inbox` / `ordering.inbox`，message id）+ `unique(order_id)` + webhook 的 `unique(provider, provider_event_id)` |
| `OrderCancelled` 比 `OrderConfirmed` 先到 | 〔補充〕Payment 先寫 `payment.cancelled_orders(order_id)` 墓碑；之後 `OrderConfirmed` 到時，看到墓碑就直接建成 `CANCELLED`（`order_cancelled = true`），不計時。**沒有墓碑機制，這個順序會留下一張永遠 `PENDING` 且可被付款的單** |
| webhook 收款比 `OrderCancelled` 先到 | T3 → `CAPTURED`，發 `PaymentCaptured`；ordering 若訂單已取消就拒絕（domain error，drop）；之後 `OrderCancelled` 到 → T10 退款 |
| webhook 收款比 `OrderCancelled` 晚到 | T8 後 `order_cancelled` 已真 → 直接 T10 退款 |
| `PaymentCaptured` 與 `PaymentExpired` 同時待處理 | ordering 的狀態機判定：先處理哪個，哪個贏；輸的那個被拒（domain error，drop）。`PaymentCaptured` 先 → `PAID`→`COMPLETED`，之後 `PaymentExpired` 因訂單非 `CONFIRMED` 被拒；`PaymentExpired` 先 → `CANCELLED` → `OrderCancelled` → payment T10 退款 |
| 舊事件晚到、版本落後 | 付款單 optimistic lock；`ErrStale` 讓 consumer retry，domain error drop |
| webhook 簽章錯 | 回 4xx、不落地、不重試；log 只寫固定分類 |

## 5. 競態表（ordering 的狀態機判定）

| 情境 | 結果 | 預定測試（切片） |
|---|---|---|
| `PaymentCaptured` 先到，訂單 `CONFIRMED` | 訂單 `PAID`→`COMPLETED`，之後的逾時與取消被拒絕 | S3 ordering consumer；S6 kind |
| 訂單已 `CANCELLED`，`PaymentCaptured` 才到 | 拒絕 `PAID`；Payment 自動全額退款（T10） | S3；S6 kind |
| 顧客在付款途中取消訂單 | Payment 收到 `OrderCancelled`：取消 provider 端付款（T7）；已收款就退款（T10） | S3；S6 kind |
| 逾時後 provider 才通知成功 | 訂單已取消就退款，**不復活訂單**；訂單尚未處理 `PaymentExpired` 時 `PaymentCaptured` 先贏則訂單 `PAID` | S3；S6 kind |

## 6. 發起方式與 RPC

- **建單**：`consume` 收到 `OrderConfirmed` 建 `PENDING` 付款單並開始計時（T1），**此時不打 provider**。
- **`StartPayment(order_id)`**：顧客呼叫才向 provider 開 hosted checkout，回傳導向 URL；重複呼叫回傳尚未過期的同一個 URL（T2）。provider 呼叫不在 DB transaction 內（`decorateNoTx`）。
- **`GetPayment`**：client 輪詢進度。
- 擁有權：付款單的 `customer_id` 來自事件；非本人（非 admin）一律 `NOT_FOUND`，同 `GetOrder` 的形狀〔補充：admin 可讀，不可 `StartPayment` 代付，第一版不開〕。
- 〔補充〕**customer 怎麼帶**：`apps/rpc` 驗完身分後，以 service key 呼叫 `payment-rpc`，customer 放在 request 的明確欄位（`customer_id`），不放 header、不轉傳使用者 credential。這代表**持有 service key 的人可以冒充任何 customer**——所以 service key 只能是 rpc 那一把（單一 key、`--require-durable` 檢查），且 `payment-rpc` 不該對外暴露 RPC 路徑（來源限制由切片 5 的 chart 處理，目前未設計，不是保證）。見 U2。

## 7. Provider、webhook 與認證邊界

### Provider

第一版只有 mock，走 application 的 `PaymentProvider` port（`CreateCheckout`、`GetPayment`、`CancelCheckout`、`Refund`、`VerifyWebhook`）。mock 要能模擬：hosted checkout、webhook 簽章、延遲、重送與亂序（測試與 kind 用）。

### Webhook

1. 普通 HTTP route，不走 Connect，也不走 API key；憑證是**簽章**，用**原始 body** 驗簽（不先 parse 再重組）。
2. 驗簽後存進 `payment.webhook_events`，`(provider, provider_event_id)` 唯一；成功就回 200，之後由 `consume` 處理。重送也回 200（已落地）。
3. 付款單狀態只往前走，晚到的舊事件忽略（§3 轉移表）。
4. 金額和幣別必須和付款單相符；不符時不進 `CAPTURED`，進 `NEEDS_REVIEW` 並告警（T4、T9）。
5. `expire` 在標記 `EXPIRED` 前先向 provider 查一次狀態，涵蓋 webhook 沒送到的情況（T5/T6）。
6. Log 只寫固定分類與 code，不寫 provider 的原始錯誤或 payload（#347）；簽章、body 與 provider 密鑰不進 log、error、metric label、span。

### `payment-rpc` 認證入口逐一列出

認證比照 `catalog-rpc`：不接 identity、沒有 `CredentialResolver`、只認靜態 `auth.api_keys`；契約不支援的 credential 來源 **fail closed 拒啟動**（#323）。

| 入口 | 路徑 | 憑證 | 為什麼 |
|---|---|---|---|
| Connect RPC（`PaymentService`） | `/api/<svc>/<Method>` 與原生路徑 | `X-API-Key`，且只有 service key `rpc`；`auth.mode` 必須 `required` | 呼叫者只有 `apps/rpc` |
| Webhook | 一條 gin route（路徑待切片 3 定） | **簽章**（provider 簽，用原始 body 驗） | provider 打不出我們的 API key，也不該有；API key 是我們發給自己服務的 credential，給 provider 等於把 service key 交出去。**gin route 不在 Connect chain 內，auth interceptor 不會保護它**，簽章是唯一一道閘 |
| `/api/healthz`、`/api/readyz`、`/api/version` | gin | 無（永遠開著） | probe；設計上 `readyz` 只看 payment 自己的 DB，不依賴 provider（同 rpc 的 `readyz` 不依賴 catalog），切片 2 補測試才算保證 |
| `/api/metrics` | gin | 無（gin route，不經 auth interceptor，見 `pkgs/platform/server/auth.go` 檔頭註解） | 同其他 app；來源限制是切片 5 chart 的事，這裡不保證 |
| `/api/docs` | gin | 無；預設關 | 同其他 app |
| 啟動拒絕（fail closed） | — | `auth.jwt` 有設、`auth.oidc.enabled`、`auth.mode` 開著但 `auth.api_keys` 空；`--require-durable` 另要求 `auth.mode: required`、`auth.api_keys` 恰好一把且名為 `rpc`、`postgres.url` 不空 | #323：傳 nil 不等於停用 |
| 沒有的入口 | — | 沒有 streaming RPC（auth 是 unary-only）；沒有 Bearer；沒有 identity key | 加了要另外包 |

`consume`、`expire` 是背景程序，沒有對外 listener；`serve` 與它們共用同一份 provider 密鑰設定，但只有 `serve` 的 webhook route 讀「驗簽金鑰」。

## 8. `ordering.require_payment` 矩陣

開關預設 `false`（切片 6 之前）；切片 6 之後**新部署**預設 `true`，範例設定與 chart 同步更新（已存在部署的 values 不自動改）。
「有付款單」＝ 該訂單在 Payment 有付款單（不看建單時間）。

| `require_payment` | 付款單 | `CompleteOrder`（`CONFIRMED` 訂單） | `CancelOrder`（`CONFIRMED` 訂單） | 預定測試 |
|---|---|---|---|---|
| `false`（預設） | 無 | **可直接完成**（現狀） | 可取消（現狀） | S1：既有 usecase 測試**在預設設定下**全綠不改；S6 加一格明確斷言預設值 |
| `false` | 有（`PENDING`/…） | 可直接完成；**付款單不被動到**（見 U1） | 可取消；Payment 收 `OrderCancelled`（T7/T10） | S3：開關 `false` 時 ordering **忽略** `PaymentCaptured`/`PaymentExpired`（drop，不取消、不 `PAID`）〔補充：這樣 payment-rpc 先上線也不改變下單行為，維持「切片 6 之前不改變現有下單行為」〕 |
| `true` | 無（含切換當下的舊訂單） | **照舊可完成**（依「有沒有付款單」判斷，不看建單時間） | 可取消 | S6：kind 切換前建的 `CONFIRMED` 單，開關打開後仍可 `CompleteOrder` |
| `true` | 有 | **拒絕**（`CONFIRMED` 只能經付款前進；錯誤碼待切片 1 定，進 `error-codes.md`）；`PAID` 後由 ordering 自動完成 | 可取消（`CONFIRMED`）；`PAID`/`COMPLETED` 拒絕 | S3、S6 |

> 「有付款單」要 ordering 知道。〔補充〕做法：`require_payment=true` 時 `CompleteOrder` 向 payment 查（經 payment 的 contract client，同 PriceQuoter 形狀）還是 ordering 自己記「已收到 `PaymentCaptured`／已發起」？兩者對 `payment-rpc` 掛掉的行為不同——**這是 U4，需要 Sean 決定**；在決定前，本表「有」那兩格的 `CompleteOrder` 行為以「拒絕」為準，但 `payment-rpc` 掛掉時的結果未定。

## 9. 切片與保證對照

| # | 切片 | 內容 | 改變現有下單行為？ |
|---|---|---|---|
| 0 | 設計文件 | 本文 | 否 |
| 1 | Ordering 契約 | `OrderConfirmed.total`、`PAID`、`OrderPaid`、`require_payment` 設定（預設 `false`）、Console 顯示 | 否（`PAID` 無人觸發） |
| 2 | Payment 核心 | `pkgs/payment`、schema、mock provider、`payment-rpc serve` | 否（沒部署） |
| 3 | Saga | `consume`、webhook 落地、`expire`、自動退款、ordering 訂閱 `PaymentCaptured`/`PaymentExpired` | 否（開關 `false` 時忽略） |
| 4 | 入口 | rpc 轉發 `StartPayment`、`GetPayment`，Console 付款頁 | 否 |
| 5 | 部署 | goreleaser 第六個 image、chart、Grafana、alert | 否 |
| 6 | 切換 | 開啟開關，kind 跑完整流程與第 5 節全部競態 | **是** |

每片一個 PR，切片 6 之前都不改變現有下單行為。

**切片 5 的順序**：image 先發版 → Sean 手動把新 GHCR package（`payment-rpc`）設成 public → chart 才能引用。bot chart bump 的常設授權 `d-20261031033617246691-4` 的匿名 image 檢查要從 5 個改成 6 個，**隨切片 5 同步改書面**（不能只改實際做法）。同時更新 `docs/release.md` 的 image 清單與「第一次 push 後改 public」那段。

### 保證 → 測試

| 保證 | 切片 | 預定測試 |
|---|---|---|
| 舊 `OrderConfirmed`（無 `total`）不建付款單、不當 0 元 | S2/S3 | consumer 測試：無 `total` → domain error + 沒有 `payment.payments` 列 |
| 付款單狀態只依 §3 表轉移，其餘拒絕 | S2 | domain 表驅動測試（`domain/*` 100% 覆蓋，CI `cover-gate`） |
| `StartPayment` 重複呼叫回同一個未過期 URL；過期後重開 | S2 | usecase 測試（mock provider 計數 `CreateCheckout` 次數） |
| `StartPayment` 的 provider 呼叫不在 DB transaction 內 | S2 | 測試：provider fake 在呼叫當下斷言沒有 open tx（觀察同一個 pool／handle） |
| 非本人 `GetPayment`/`StartPayment` → `NOT_FOUND` | S2 | usecase 測試 + rpc 層測試 |
| webhook 簽章錯 → 4xx、不落地；對簽章用原始 body | S3 | HTTP 測試：改一個 byte、重排 JSON key 各一例 |
| webhook 重送不重複處理 | S3 | `(provider, provider_event_id)` 唯一 + 重送測試（Postgres） |
| 金額／幣別不符 → `NEEDS_REVIEW`、告警、不發 `PaymentCaptured` | S3 | consumer 測試（金額、幣別各一例）|
| `expire` 先查 provider：provider 已收款 → `CAPTURED`（T6），否則 `EXPIRED` | S3 | `expire` 測試（mock provider 兩種回應） |
| `PaymentCaptured` 與 `OrderCancelled` 任何順序都收斂到退款（T10） | S3 | 兩個順序 + 墓碑（`OrderCancelled` 先於 `OrderConfirmed`）的 consumer 測試，Postgres |
| 退款冪等：同一付款單重送退款 provider 只收到一次實質退款 | S3 | mock provider 以冪等 key 去重；測試重送 `PaymentRefundRequested` |
| 退款失敗有界重試，超過次數進 poison + 告警 | S3 | fault injection：provider `Refund` 一律失敗，斷言 retry 次數上限與 poison 列；positive control：恢復後成功 |
| `PAID` 後 `CancelOrder` 被拒；`CANCELLED` 不復活 | S1/S3 | domain 與 usecase 測試 |
| 開關 `false`：ordering 忽略 payment 事件；`CompleteOrder` 行為不變 | S1/S3 | **預設設定**下的 usecase 測試（不顯式設 `false`，驗預設值本身） |
| 開關 `true`：未付款（有付款單）`CompleteOrder` 被拒；舊訂單（無付款單）可完成 | S6 | kind 端到端 + usecase 測試 |
| `apps/rpc` 不載入 payment 領域程式 | S4 | archtest／depguard 負例（比照 `apps/rpc/catalog_split_test.go`） |
| `payment-rpc` 掛掉：付款操作回 503，下單與預留照常 | S4/S6 | kind 故障演練：停 payment-rpc → `CreateOrder` 成功且到 `CONFIRMED`；`StartPayment` → `UNAVAILABLE`；恢復後同一 pod 可繼續 |
| `payment-rpc` fail closed：`auth.jwt`／`auth.oidc` 拒啟動；`--require-durable` 條件 | S2/S5 | 比照 `apps/catalog-rpc/durable_test.go`，「單獨配置」與「與支援來源並存」兩種負例 |
| webhook route 不受 API key 影響、簽章是唯一閘 | S3 | HTTP 測試：`auth.mode: required` 下無 `X-API-Key` 但簽章正確 → 200；簽章錯 → 4xx |
| 第 5 節四個競態在真環境成立 | S6 | kind 全流程（mock provider 的延遲／重送／亂序） |

## 10. 儲存（`payment` schema）

`payment.payments`、`payment.webhook_events`（`unique(provider, provider_event_id)`、原始 body 與簽章結果、處理狀態）、`payment.cancelled_orders`（墓碑）、`payment.events`／`payment.inbox`／`payment.poison`（同其他 BC 的 outbox／inbox／poison 形狀）、`payment.schema_migrations`。`apps/migrate` 收集 payment 的 `Migration()`。`postgres.url` 空 → in-memory（`task run`），遵守同一份 `Save` 合約。

## 11. 不做

admin 退款、部分退款、`COMPLETED` 之後的退款（先到 provider 後台處理）、真 provider（第一版只有 mock）、Fulfillment（所以 `PAID` 不停留）。

## 12. 未決事項（需要 Sean 決定，本文沒有擅自調整設計）

| # | 問題 | 為什麼卡住 | 建議 |
|---|---|---|---|
| U1 | **開關 `false`（或切換後的舊單）下，訂單被 `CompleteOrder` 完成後，若該訂單有 `PENDING` 付款單，顧客之後仍可付款，錢收了訂單卻不能 `PAID`，也沒有 `OrderCancelled` 觸發退款** | issue 規定 payment 只訂閱 `OrderConfirmed`、`OrderCancelled`；`CompleteOrder` 在 `false` 時可直接完成 | payment 另訂閱 `OrderCompleted`：`PENDING` → `CANCELLED`（並取消 provider 端付款）。需改 issue §5 的訂閱清單。若選擇不做，要明寫「`false` 下已完成訂單的付款單仍可被付款」為已知限制 |
| U2 | **`customer_id` 由 rpc 以 request 欄位傳給 payment-rpc**，持 service key 者可冒充任何顧客 | issue 說「帶上 customer」但沒定機制 | 維持單一 service key，來源限制交給切片 5 的 chart；不加簽章 header，除非 Sean 要求 |
| U3 | **poison 與 prune 的入口**：`worker poison`／`worker prune`／`NewPoisonService` 的 `schemas` 清單不含 payment（worker 不載入 payment）；payment 的 outbox／inbox／`webhook_events`／poison 表沒有 retention 與營運入口 | issue 的 `payment-rpc` 只有 `serve`／`consume`／`expire` 三個子命令 | 讓 `schemas` 清單（純表名，不載入領域程式）加 `payment`，或給 `payment-rpc` 加 `poison`／`prune` 子命令。需回 Sean 定；定之前 retention 與 `webhook_events` 的清理不是保證 |
| U4 | **ordering 怎麼知道「有付款單」**（§8 注） | 影響 `payment-rpc` 掛掉時 `CompleteOrder` 的行為 | 傾向 ordering 自己記（收到 `PaymentCaptured`／`OrderConfirmed` 已發出且開關為真即視為有），不在 `CompleteOrder` 路徑上同步問 payment，避免 payment 掛掉拖垮 ordering；代價是 ordering 要多存一個旗標 |
| U5 | `FAILED` 狀態、`NEEDS_REVIEW` 的人工出口（誰、怎麼把它推到 `CAPTURED`／退款）| issue 沒有 admin 操作 | 第一版只告警，不提供 RPC；手動處理走 provider 後台與 SQL，記入 runbook（切片 5） |
| U6 | 開關 `false` 時 `StartPayment` 入口（切片 4 的 rpc 轉發與 Console 付款頁）該不該可用 | `false` 時 ordering 忽略 `PaymentCaptured`，顧客若付了款，訂單不會 `PAID` | 建議 `false` 時 rpc 的 `StartPayment` 回 `FAILED_PRECONDITION`、Console 不顯示付款頁；需 Sean 確認，因為這是切片 4 的行為而非 issue 寫明的 |
