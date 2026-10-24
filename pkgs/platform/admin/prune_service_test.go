package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// Integration (pgtest): Prune through a real server with auth required. Two
// BCs so the inbox exclusion is cross-BC: a message parked in B's poison
// table keeps A's inbox row.
func TestPruneRPC(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, dbCleanup, err := platformpg.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCleanup()
	sqlDB, _ := db.DB()
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS prunea CASCADE", "CREATE SCHEMA prunea",
		"DROP SCHEMA IF EXISTS pruneb CASCADE", "CREATE SCHEMA pruneb",
		"CREATE TABLE prunea.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
		"CREATE TABLE pruneb.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
	} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	const topic, lonely = "prunea.v1.Ping", "prunea.v1.Nobody"
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range []string{topic, lonely, eventbus.PoisonTopic("prunea"), eventbus.PoisonTopic("pruneb")} {
		if err := init.SubscribeInitialize(tp); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := pub.Publish(topic, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := pub.Publish(lonely, message.NewMessage(watermill.NewUUID(), []byte("p"))); err != nil {
		t.Fatal(err)
	}
	// Parked in B's poison table under this id: A's inbox row for it must survive.
	parked := message.NewMessage(watermill.NewUUID(), []byte("p"))
	parked.Metadata.Set(middleware.PoisonedTopicKey, topic)
	if err := pub.Publish(eventbus.PoisonTopic("pruneb"), parked); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE " + eventbus.TableName(topic) + " SET created_at = created_at - interval '48 hours'",
		"UPDATE " + eventbus.TableName(lonely) + " SET created_at = created_at - interval '48 hours'",
		// A live group that read everything and a stale one that never will.
		"INSERT INTO " + eventbus.OffsetsTableName(topic) + ` (consumer_group, offset_acked, last_processed_transaction_id)
			SELECT 'live.prunea', max("offset"), max(transaction_id) FROM ` + eventbus.TableName(topic) + ` UNION ALL SELECT 'test-old.prunea', 0, '0'`,
		"INSERT INTO prunea.inbox (message_id, consumed_at) VALUES ('" + parked.UUID + "', now() - interval '48 hours'), ('" + watermill.NewUUID() + "', now() - interval '48 hours'), ('" + watermill.NewUUID() + "', now())",
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	count := func(tbl string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	auth := config.Auth{Mode: config.AuthRequired, AdminRole: "admin", APIKeys: "ops=ops-key-0123456789abcdef:admin, viewer=viewer-key-0123456789"}
	svc := admin.NewPoisonService(db, auth, []string{"prunea", "pruneb"})
	srv, err := server.New(config.HTTP{Addr: ":0"}, auth, tel.Logger, []server.Route{svc}, nil, nil, tel)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	withKey := func(key string) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(server.APIKeyHeader, key)
				return next(c, req)
			}
		}))
	}
	viewer := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL, withKey("viewer-key-0123456789"))
	ops := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL, withKey("ops-key-0123456789abcdef"))
	day := durationpb.New(24 * time.Hour)

	_, err = viewer.Prune(c, connect.NewRequest(&adminv1.PruneRequest{OlderThan: day, DryRun: true}))
	if code, ec := errCode(err); code != connect.CodePermissionDenied || ec != server.PermissionDeniedCode {
		t.Fatalf("viewer = %v", err)
	}
	_, err = ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{DryRun: true}))
	if code, ec := errCode(err); code != connect.CodeInvalidArgument || ec != server.ValidationErrorCode {
		t.Fatalf("no older_than = %v", err)
	}
	_, err = ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{OlderThan: durationpb.New(-time.Hour), DryRun: true}))
	if code, ec := errCode(err); code != connect.CodeInvalidArgument || ec != server.ValidationErrorCode {
		t.Fatalf("negative older_than = %v", err)
	}
	_, err = ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{BoundedContext: "billing", OlderThan: day, DryRun: true}))
	if code, ec := errCode(err); code != connect.CodeInvalidArgument || ec != "admin.unknown_bounded_context" {
		t.Fatalf("unknown bc = %v", err)
	}

	// Dry run over both BCs: the stale group holds the topic back, the
	// published-only table is skipped, the inbox would lose one row (the
	// parked id is kept); nothing is deleted.
	dry, err := ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{OlderThan: day, DryRun: true}))
	if err != nil {
		t.Fatal(err)
	}
	tables := dry.Msg.GetTables()
	if len(tables) != 4 || dry.Msg.GetTotal() != 1 {
		t.Fatalf("dry run = %v", dry.Msg)
	}
	if tbl := tables[0]; tbl.GetTable() != eventbus.TableName(lonely) || tbl.GetRows() != 0 || tbl.GetSkipped() == "" {
		t.Fatalf("published-only table = %v", tbl)
	}
	if tbl := tables[1]; tbl.GetTable() != eventbus.TableName(topic) || tbl.GetRows() != 0 || len(tbl.GetBlockers()) != 1 ||
		tbl.GetBlockers()[0].GetConsumerGroup() != "test-old.prunea" || tbl.GetBlockers()[0].GetUnread() != 3 {
		t.Fatalf("held topic = %v", tbl)
	}
	if tbl := tables[2]; tbl.GetTable() != "prunea.inbox" || tbl.GetRows() != 1 {
		t.Fatalf("inbox = %v", tbl)
	}
	if tbl := tables[3]; tbl.GetTable() != "pruneb.inbox" || tbl.GetRows() != 0 {
		t.Fatalf("other inbox = %v", tbl)
	}
	if n := count(eventbus.TableName(topic)); n != 3 {
		t.Fatalf("dry run deleted: %d rows left", n)
	}
	if n := count("prunea.inbox"); n != 3 {
		t.Fatalf("dry run deleted inbox: %d rows left", n)
	}

	// Forgetting the stale group in a dry run reports it, keeps its cursor.
	dry, err = ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{BoundedContext: "prunea", OlderThan: day, DryRun: true, ForgetGroups: []string{"test-old.prunea", "never"}}))
	if err != nil || len(dry.Msg.GetTables()) != 3 || dry.Msg.GetTotal() != 4 {
		t.Fatalf("dry forget = %v, %v", dry.Msg, err)
	}
	if tbl := dry.Msg.GetTables()[1]; tbl.GetRows() != 3 || len(tbl.GetBlockers()) != 0 || len(tbl.GetForgotten()) != 1 || tbl.GetForgotten()[0] != "test-old.prunea" {
		t.Fatalf("dry forget topic = %v", tbl)
	}
	if n := count(eventbus.OffsetsTableName(topic)); n != 2 {
		t.Fatalf("dry forget dropped a cursor: %d left", n)
	}

	// The real run, one BC: cursor gone, three rows and one inbox row deleted,
	// the parked id's inbox row and the published-only table untouched.
	real, err := ops.Prune(c, connect.NewRequest(&adminv1.PruneRequest{BoundedContext: "prunea", OlderThan: day, ForgetGroups: []string{"test-old.prunea"}, BatchSize: 2}))
	if err != nil || real.Msg.GetTotal() != 4 {
		t.Fatalf("prune = %v, %v", real.Msg, err)
	}
	if n := count(eventbus.OffsetsTableName(topic)); n != 1 {
		t.Fatalf("cursor not forgotten: %d left", n)
	}
	if n := count(eventbus.TableName(topic)); n != 0 {
		t.Fatalf("topic rows left: %d", n)
	}
	if n := count(eventbus.TableName(lonely)); n != 1 {
		t.Fatalf("published-only table pruned: %d left", n)
	}
	var kept int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM prunea.inbox WHERE message_id = ?", parked.UUID).Scan(&kept).Error; err != nil || kept != 1 {
		t.Fatalf("parked id's inbox row = %d, %v", kept, err)
	}
	if n := count("prunea.inbox"); n != 2 {
		t.Fatalf("inbox rows left: %d", n)
	}

	nodb := admin.NewPoisonService(nil, config.Auth{Mode: config.AuthOff}, []string{"prunea"})
	_, err = nodb.Prune(c, connect.NewRequest(&adminv1.PruneRequest{OlderThan: day, DryRun: true}))
	if code, ec := errCode(err); code != connect.CodeFailedPrecondition || ec != "admin.no_database" {
		t.Fatalf("no db = %v", err)
	}
}
