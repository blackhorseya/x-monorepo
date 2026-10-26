# Observability

<sub>← [README](../README.md)</sub>

## Observability：OTel → LGTM

`platform/otel.New(cfg.OTel, cfg.Log)` 回傳 `*otel.Providers`（TracerProvider、MeterProvider、Propagator、root `*slog.Logger`、
`/api/metrics` handler），不碰 global。程式碼只用 OTel API instrument，Prometheus 只是輸出格式：

- **metrics 永遠開**：MeterProvider 掛一個 Prometheus reader，`server` 把它放在 `GET /api/metrics`（沒 collector 也能 scrape）。
  `usecasex.Metrics` 記 `usecase.duration`（exposition：`usecase_duration_seconds{usecase, kind}`），
  `otelconnect` 產 `rpc.server.call.duration`，`contrib/instrumentation/runtime` 產 Go runtime metrics，
  auth interceptor 記 `auth.decisions`（`auth_decisions_total{result, method}`：`ok` / `public` / `anonymous` / `missing` / `invalid` × `api_key` / `jwt` / `none`——
  RPC status 只會說 `unauthenticated`，分不出「沒帶」和「帶了被拒」；label 只有這兩個有限集合，沒有 subject）
- **worker 的 backlog**（`platform/eventbus`，`apps/worker` 註冊）：`eventbus.consumer.duration{topic, result}` 每次 handler
  attempt 一個 sample（retry 也算，被 park 的訊息就是它最後一次 `failed`）；`eventbus.Lag` 掛 observable gauge，每次 collect
  才去查：`eventbus_consumer_lag{topic, consumer_group, broker}` = 該 group 還沒 ack 的訊息數——Postgres 用和 watermill-sql
  subscriber 一樣的謂詞（cursor 之後、且 `transaction_id < snapshot xmin`，所以只算「現在真的讀得到」的），Redis 是 XINFO 的
  `lag + pending`；`eventbus_consumer_oldest_age_seconds` 是最老那筆等了多久（在 SQL / 用 Redis server 的時鐘算，不吃本機時區與時鐘偏移）；
  `eventbus_poison_messages{topic}` 是 `<bc>.poison` 表的 row 數（沒人消費，所以也是累計）。redis 模式多一組
  `consumer_group="forwarder"` 的 Postgres cursor（outbox → stream 那一跳）。有 DB 時同一個 callback 再報 retention 狀態：
  `eventbus_outbox_rows{table}` / `eventbus_outbox_oldest_age_seconds{table}` 是每張 outbox 表的大小與最老一列的年齡，
  `eventbus_outbox_held{table}` / `eventbus_outbox_oldest_held_age_seconds{table}` 是 offsets 表裡**任一** group 還沒 ack 的列
  （prune 不管多老都留著；held 的年齡一直長但 consumer lag 很低 = 有個沒人跑的 stale group），`eventbus_inbox_rows{schema}` /
  `eventbus_inbox_oldest_age_seconds{schema}` 是 `<bc>.inbox`。這些是 `count(*)` 全表掃，表很大時把 OTLP 的 collect 間隔拉長。
  每個 replica 都回報同一組數字：**用 `max`，不要 `sum`**。
  worker 沒有 HTTP server，這些只走 OTLP（60s 一次）
- **saga 的漏斗**（各 BC 的 `adapters/metrics`，`apps/worker` 和 lag gauge 一起註冊，只在有 DB 時）：`ordering_orders{status}` /
  `inventory_reservations{status}` 是每個 status 的筆數，`ordering_orders_oldest_age_seconds{status}` / `inventory_reservations_oldest_age_seconds{status}`
  是該 status 最老一筆距上次狀態變更的秒數（每個已知 status 都有 series，空的報 0——alert 要看到 0 不是 no data）。
  每次 scrape 一句 `GROUP BY status`（`OrderStats` / `ReservationStats` use case → `Repository.CountByStatus`），
  每個 replica 報一樣的數字，`max` 不 `sum`。PENDING 老 = StockReserved 或缺貨的取消都沒回到 ordering（alert `x-monorepo-saga-pending`）；
  RESERVED 老 = 庫存被沒走完的訂單扣著，超過 TTL 的 1.5× 就是 `worker expire` 沒跑（alert `x-monorepo-reservation-expiry`）；EXPIRED 在長 = TTL 在殺單，看 TTL 是不是太短。
- **有 `otel.endpoint` 才推 OTLP**：traces 走 batcher、metrics 多一個 periodic reader、logs 走 batch processor；`otelconnect`
  interceptor 掛最外層，每個 RPC 一個 span。endpoint 空 = tracer no-op、logger 只寫 stdout、不 dial
- **span 的來源**：RPC 由 `otelconnect`；`/api/docs`、`/api/openapi/*` 等普通 route 由 `otelgin`（filter 掉 RPC prefix、
  `/api/healthz`、`/api/readyz`、`/api/metrics`，不會重複）；每條 SQL 由 `platform/postgres` 自己的小 gorm plugin 掛在 request span 底下（只有 SQL 形狀，不帶變數；不用
  `gorm.io/plugin/opentelemetry`，它會把 ClickHouse / MySQL driver 拖進 binary）
- **logs**：root logger 永遠寫 stdout（`log.format` / `log.level`），有 endpoint 時再 fan-out 一份給 `otelslog` bridge → Loki，
  同一個 level gate。`contextx.Contextx` 的 `Info/Warn/…` 會把 context 帶給 handler，所以 bridge 自動附 `trace_id` / `span_id`
  （stdout 那份靠 interceptor 的 `trace_id` attr）。Loki 裡 attr 是 structured metadata：查 `{service_name="rpc"} | trace_id="…"`，
  不要用 `|=` line filter（比不到）
- **resource**：明確設 `service.name`（= Prometheus `job`）與 `service.instance.id`（= `instance`）——OTLP→Prometheus
  只有這兩個 attribute 會變成 series label，少了 instance.id 多 replica 會寫同一條 series、counter 亂跳

本機：`task compose-up` 連 `grafana/otel-lgtm` 一起起（Grafana `http://localhost:3000`，匿名 admin；OTLP gRPC :4317），
`configs/local.yaml` 的 `otel.endpoint` 已指向它。
