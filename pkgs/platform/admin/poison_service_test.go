package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/ThreeDotsLabs/watermill"
	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"

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

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	os.Exit(code)
}

const topic = "admintest.v1.Ping"

func errCode(err error) (connect.Code, string) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0, ""
	}
	return ce.Code(), ce.Meta().Get("x-error-code")
}

// Integration (pgtest): the service through a real server.New with auth
// required — role gate, then list / get / requeue / drop on a poison table.
func TestPoisonService(t *testing.T) {
	c := context.Background()
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
	for _, q := range []string{"DROP SCHEMA IF EXISTS admintest CASCADE", "CREATE SCHEMA admintest"} {
		if _, err := sqlDB.ExecContext(c, q); err != nil {
			t.Fatal(err)
		}
	}
	init, err := wsql.NewSubscriber(wsql.BeginnerFromStdSQL(sqlDB), wsql.SubscriberConfig{
		ConsumerGroup:  "init",
		SchemaAdapter:  wsql.DefaultPostgreSQLSchema{GenerateMessagesTableName: eventbus.TableName, GeneratePayloadType: func(string) string { return "BYTEA" }},
		OffsetsAdapter: wsql.DefaultPostgreSQLOffsetsAdapter{GenerateMessagesOffsetsTableName: eventbus.OffsetsTableName},
	}, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range []string{topic, eventbus.PoisonTopic("admintest")} {
		if err := init.SubscribeInitialize(tp); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	park := func(payload string) string {
		msg := message.NewMessage(watermill.NewUUID(), []byte(payload))
		msg.Metadata.Set(middleware.PoisonedTopicKey, topic)
		msg.Metadata.Set(middleware.ReasonForPoisonedKey, "boom")
		if err := pub.Publish(eventbus.PoisonTopic("admintest"), msg); err != nil {
			t.Fatal(err)
		}
		return msg.UUID
	}

	auth := config.Auth{Mode: config.AuthRequired, AdminRole: "admin", APIKeys: "ops=ops-key-0123456789abcdef:admin, viewer=viewer-key-0123456789"}
	svc := admin.NewPoisonService(db, auth, []string{"admintest"})
	srv, err := server.New(config.HTTP{Addr: ":0"}, auth, tel.Logger, []server.Route{svc}, nil, nil, tel, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	withKey := func(key string) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if key != "" {
					req.Header().Set(server.APIKeyHeader, key)
				}
				return next(c, req)
			}
		}))
	}
	anon := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL)
	viewer := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL, withKey("viewer-key-0123456789"))
	ops := adminv1connect.NewAdminServiceClient(http.DefaultClient, ts.URL, withKey("ops-key-0123456789abcdef"))

	// Gate: anonymous → unauthenticated, a principal without the role → permission denied.
	_, err = anon.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{}))
	if code, ec := errCode(err); code != connect.CodeUnauthenticated || ec != server.UnauthenticatedCode {
		t.Fatalf("anonymous = %v", err)
	}
	_, err = viewer.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{}))
	if code, ec := errCode(err); code != connect.CodePermissionDenied || ec != server.PermissionDeniedCode {
		t.Fatalf("viewer = %v", err)
	}

	// Validation and unknown BC are client errors.
	_, err = ops.RequeuePoison(c, connect.NewRequest(&adminv1.RequeuePoisonRequest{BoundedContext: "admintest"}))
	if code, ec := errCode(err); code != connect.CodeInvalidArgument || ec != server.ValidationErrorCode {
		t.Fatalf("neither offsets nor all = %v", err)
	}
	_, err = ops.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{BoundedContext: "billing"}))
	if code, ec := errCode(err); code != connect.CodeInvalidArgument || ec != "admin.unknown_bounded_context" {
		t.Fatalf("unknown bc = %v", err)
	}

	one, two := park("one"), park("two")
	list, err := ops.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{}))
	if err != nil || len(list.Msg.GetMessages()) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	first := list.Msg.GetMessages()[0]
	if first.GetUuid() != one || first.GetBoundedContext() != "admintest" || first.GetTopic() != topic || first.GetReason() != "boom" || len(first.GetPayload()) != 0 {
		t.Fatalf("first = %v", first)
	}
	got, err := ops.GetPoison(c, connect.NewRequest(&adminv1.GetPoisonRequest{BoundedContext: "admintest", Offset: first.GetOffset()}))
	if err != nil || string(got.Msg.GetMessage().GetPayload()) != "one" || got.Msg.GetMessage().GetMetadata()[middleware.PoisonedTopicKey] != topic {
		t.Fatalf("get = %v, %v", got, err)
	}
	_, err = ops.GetPoison(c, connect.NewRequest(&adminv1.GetPoisonRequest{BoundedContext: "admintest", Offset: 999999}))
	if code, ec := errCode(err); code != connect.CodeNotFound || ec != "admin.poison_not_found" {
		t.Fatalf("get missing = %v", err)
	}

	req, err := ops.RequeuePoison(c, connect.NewRequest(&adminv1.RequeuePoisonRequest{BoundedContext: "admintest", Offsets: []int64{first.GetOffset()}}))
	if err != nil || req.Msg.GetCount() != 1 {
		t.Fatalf("requeue = %v, %v", req, err)
	}
	var n int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM "+eventbus.TableName(topic)+" WHERE uuid = ?", one).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("requeued row = %d, %v", n, err)
	}
	drop, err := ops.DropPoison(c, connect.NewRequest(&adminv1.DropPoisonRequest{BoundedContext: "admintest", All: true}))
	if err != nil || drop.Msg.GetCount() != 1 {
		t.Fatalf("drop all = %v, %v", drop, err)
	}
	list, err = ops.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{BoundedContext: "admintest", Limit: 10}))
	if err != nil || len(list.Msg.GetMessages()) != 0 {
		t.Fatalf("list after drop = %v, %v (two=%s)", list, err, two)
	}

	// No database: a clear precondition error, not a nil-pointer panic.
	nodb := admin.NewPoisonService(nil, config.Auth{Mode: config.AuthOff}, []string{"admintest"})
	_, err = nodb.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{}))
	if code, ec := errCode(err); code != connect.CodeFailedPrecondition || ec != "admin.no_database" {
		t.Fatalf("no db = %v", err)
	}
}
