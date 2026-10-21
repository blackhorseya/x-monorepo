package eventbus_test

import (
	"context"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): a row is pruned only when it is old enough AND every
// consumer group with a cursor has acked it — two groups prove the "every".
func TestPrune(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sqlDB, _ := db.DB()
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS prunetest CASCADE", "CREATE SCHEMA prunetest",
		"CREATE TABLE prunetest.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
	} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	const topic, lonely = "prunetest.v1.Ping", "prunetest.v1.Nobody"
	poisonTopic := eventbus.PoisonTopic("prunetest")
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range []string{topic, lonely, poisonTopic} {
		if err := init.SubscribeInitialize(tp); err != nil {
			t.Fatal(err)
		}
	}
	tables, err := eventbus.TopicTables(c, db, "prunetest")
	if err != nil || len(tables) != 2 || tables[0] != eventbus.TableName(lonely) || tables[1] != eventbus.TableName(topic) {
		t.Fatalf("TopicTables = %v, %v (offsets and poison tables must be left out)", tables, err)
	}

	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 4)
	publish := func(from, to int) {
		for i := from; i < to; i++ {
			ids[i] = watermill.NewUUID()
			if err := pub.Publish(topic, message.NewMessage(ids[i], []byte("p"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := pub.Publish(lonely, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
		t.Fatal(err)
	}

	// consume acks n messages of topic for group and waits until the cursor
	// reaches offset upTo. watermill-sql reads what is there in one
	// transaction and commits the cursor after the whole batch is acked, so a
	// group only ever acks everything published before it subscribed; the
	// ack's commit also rides the subscribe context, so stop only afterwards.
	consume := func(group string, n int, upTo int64) {
		t.Helper()
		sub, err := eventbus.NewPostgresSubscriber(db, group, eventbus.FromBeginning, tel.Logger)
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(c)
		defer stop()
		msgs, err := sub.Subscribe(subCtx, topic)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			select {
			case m := <-msgs:
				m.Ack()
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: message %d never arrived", group, i)
			}
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			var acked int64
			if err := db.WithContext(c).Raw("SELECT coalesce(max(offset_acked), 0) FROM "+eventbus.OffsetsTableName(topic)+" WHERE consumer_group = ?", group).Scan(&acked).Error; err != nil {
				t.Fatal(err)
			}
			if acked >= upTo {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: cursor stuck at %d, want %d", group, acked, upTo)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// g2 sees only the first message, g1 all four.
	publish(0, 1)
	consume("g2", 1, 1)
	publish(1, 4)
	consume("g1", 4, 4)

	// Nothing is old enough yet: even fully acked rows stay.
	opts := eventbus.PruneOptions{OlderThan: time.Hour}
	if res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), opts); err != nil || res.Rows != 0 || res.Skipped != "" {
		t.Fatalf("young rows: %+v, %v", res, err)
	}
	for _, tbl := range []string{eventbus.TableName(topic), eventbus.TableName(lonely)} {
		if err := db.WithContext(c).Exec("UPDATE " + tbl + " SET created_at = created_at - interval '2 hours'").Error; err != nil {
			t.Fatal(err)
		}
	}
	// Dry run counts what a real run would delete: only the row both groups acked.
	if res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), eventbus.PruneOptions{OlderThan: time.Hour, DryRun: true}); err != nil || res.Rows != 1 {
		t.Fatalf("dry run: %+v, %v", res, err)
	}
	count := func(tbl string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(eventbus.TableName(topic)); n != 4 {
		t.Fatalf("dry run deleted: %d rows left", n)
	}
	opts.BatchSize = 1 // exercise the batch loop
	if res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), opts); err != nil || res.Rows != 1 {
		t.Fatalf("prune: %+v, %v", res, err)
	}
	var left []string
	if err := db.WithContext(c).Raw("SELECT uuid FROM " + eventbus.TableName(topic) + ` ORDER BY "offset"`).Scan(&left).Error; err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 || left[0] != ids[1] || left[2] != ids[3] {
		t.Fatalf("left = %v, want %v (only the first, acked by both, goes)", left, ids[1:])
	}
	// g2 catches up, but a group that will never run again (its cursor sits
	// at zero) holds everything back; the result names it. Forgetting it in a
	// dry run reports what would go without touching its cursor; the real run
	// drops the cursor and takes the rest.
	consume("g2", 3, 4)
	if err := db.WithContext(c).Exec("INSERT INTO " + eventbus.OffsetsTableName(topic) + " (consumer_group, offset_acked, last_processed_transaction_id) VALUES ('stale', 0, '0')").Error; err != nil {
		t.Fatal(err)
	}
	res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), opts)
	if err != nil || res.Rows != 0 || len(res.Blockers) != 1 || res.Blockers[0] != (eventbus.Blocker{Group: "stale", Unread: 3}) {
		t.Fatalf("held back by a stale group: %+v, %v", res, err)
	}
	forget := eventbus.PruneOptions{OlderThan: time.Hour, ForgetGroups: []string{"stale", "never-existed"}, DryRun: true}
	res, err = eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), forget)
	if err != nil || res.Rows != 3 || len(res.Blockers) != 0 || len(res.Forgotten) != 1 || res.Forgotten[0] != "stale" {
		t.Fatalf("forget (dry run): %+v, %v", res, err)
	}
	var staleRows int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM " + eventbus.OffsetsTableName(topic) + " WHERE consumer_group = 'stale'").Scan(&staleRows).Error; err != nil || staleRows != 1 {
		t.Fatalf("dry run touched the cursor: %d, %v", staleRows, err)
	}
	forget.DryRun = false
	res, err = eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), forget)
	if err != nil || res.Rows != 3 || len(res.Forgotten) != 1 || count(eventbus.TableName(topic)) != 0 {
		t.Fatalf("forget: %+v, %v", res, err)
	}
	if err := db.WithContext(c).Raw("SELECT count(*) FROM " + eventbus.OffsetsTableName(topic) + " WHERE consumer_group = 'stale'").Scan(&staleRows).Error; err != nil || staleRows != 0 {
		t.Fatalf("stale cursor still there: %d, %v", staleRows, err)
	}

	// A table nobody has a cursor on is skipped, unless asked for.
	if res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(lonely), opts); err != nil || res.Rows != 0 || res.Skipped == "" {
		t.Fatalf("lonely: %+v, %v", res, err)
	}
	if res, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(lonely), eventbus.PruneOptions{OlderThan: time.Hour, Unconsumed: true}); err != nil || res.Rows != 1 || res.Skipped != "" {
		t.Fatalf("lonely --unconsumed: %+v, %v", res, err)
	}
	if _, err := eventbus.PruneTopicTable(c, db, eventbus.TableName(topic), eventbus.PruneOptions{}); err == nil {
		t.Fatal("zero older_than must be refused")
	}

	// Inbox: old rows go, except one whose id is parked in a poison table.
	parked := watermill.NewUUID()
	if err := pub.Publish(poisonTopic, message.NewMessage(parked, []byte("p"))); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parked, watermill.NewUUID(), watermill.NewUUID()} {
		if err := db.WithContext(c).Exec("INSERT INTO prunetest.inbox (message_id, consumed_at) VALUES (?, now() - interval '2 hours')", id).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.WithContext(c).Exec("INSERT INTO prunetest.inbox (message_id) VALUES (?)", watermill.NewUUID()).Error; err != nil {
		t.Fatal(err)
	}
	if res, err := eventbus.PruneInbox(c, db, "prunetest", time.Hour, []string{"prunetest"}, true); err != nil || res.Rows != 2 || count("prunetest.inbox") != 4 {
		t.Fatalf("inbox dry run: %+v, %v", res, err)
	}
	if res, err := eventbus.PruneInbox(c, db, "prunetest", time.Hour, []string{"prunetest"}, false); err != nil || res.Rows != 2 || count("prunetest.inbox") != 2 {
		t.Fatalf("inbox prune: %+v, %v", res, err)
	}
	var keptParked int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM prunetest.inbox WHERE message_id = ?", parked).Scan(&keptParked).Error; err != nil || keptParked != 1 {
		t.Fatalf("parked id pruned from the inbox: %d, %v", keptParked, err)
	}
}
