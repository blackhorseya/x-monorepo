package ordering_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/consumer"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// ---------------------------------------------------------------------------------------------
// #427 slice 2, the read side of the order history, from the real provider (decorators, ownership
// policy) over memory and real Postgres.
// ---------------------------------------------------------------------------------------------

// repoSpy counts which lookup the use case chose: the owner condition must ride in the query.
type repoSpy struct {
	order.Repository
	byID, byIDForCustomer atomic.Int32
}

func (r *repoSpy) FindByID(c context.Context, id order.ID) (*order.Order, error) {
	r.byID.Add(1)

	return r.Repository.FindByID(c, id)
}

func (r *repoSpy) FindByIDForCustomer(c context.Context, id order.ID, customer string) (*order.Order, error) {
	r.byIDForCustomer.Add(1)

	return r.Repository.FindByIDForCustomer(c, id, customer)
}

// listSpy counts reads of the history table.
type listSpy struct {
	port.HistoryStore
	lists atomic.Int32
}

func (l *listSpy) List(c context.Context, id order.ID) ([]order.HistoryEntry, error) {
	l.lists.Add(1)

	return l.HistoryStore.List(c, id)
}

func (s *hstack) historyUC() rpc.GetOrderHistoryUC {
	s.t.Helper()
	rec, err := usecasex.NewRecorder(sdkmetric.NewMeterProvider())
	if err != nil {
		s.t.Fatal(err)
	}

	return ordering.ProvideGetOrderHistory(s.repo, s.history, ordering.ProvideOwnership(config.Auth{AdminRole: adminRole}), rec)
}

func readHistory(t *testing.T, ctx context.Context, uc rpc.GetOrderHistoryUC, id string) *dto.GetOrderHistoryOutput {
	t.Helper()
	out, err := uc.Execute(ctx, dto.GetOrderHistoryInput{OrderID: id})
	if err != nil || out == nil {
		t.Fatalf("GetOrderHistory(%s) = %+v, %v", id, out, err)
	}

	return out
}

// Whose order it is is decided where the order is selected (FindByIDForCustomer), before the history
// table is read at all: a foreign order costs what a missing one costs, and the answer is the same.
func TestGetOrderHistory_OwnershipIsDecidedAtSelection(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		var rs *repoSpy
		var ls *listSpy
		s := mk(stackOpts{
			repo:      func(r order.Repository) order.Repository { rs = &repoSpy{Repository: r}; return rs },
			wrapStore: func(h port.HistoryStore) port.HistoryStore { ls = &listSpy{HistoryStore: h}; return ls },
		})
		s.clock.set(day(10))
		s.newOrder(asCustomer("alice"), "o-alice", "alice")
		uc := s.historyUC()
		reset := func() { rs.byID.Store(0); rs.byIDForCustomer.Store(0); ls.lists.Store(0) }

		// Positive control: the owner reads their own history, through the owner-bound lookup.
		reset()
		if got := readHistory(t, asCustomer("alice"), uc, "o-alice"); len(got.Entries) != 1 || got.Entries[0].To != "PENDING" {
			t.Fatalf("alice's own history = %+v, want the one CREATED entry", got.Entries)
		}
		if rs.byIDForCustomer.Load() != 1 || rs.byID.Load() != 0 || ls.lists.Load() != 1 {
			t.Errorf("owner read: byIDForCustomer=%d byID=%d lists=%d, want 1/0/1", rs.byIDForCustomer.Load(), rs.byID.Load(), ls.lists.Load())
		}

		// A foreign order and a missing one: NOT_FOUND, and the history table is never read.
		for _, id := range []string{"o-alice", "o-never"} {
			reset()
			out, err := uc.Execute(asCustomer("bob"), dto.GetOrderHistoryInput{OrderID: id})
			if !errors.Is(err, order.ErrNotFound) || out != nil {
				t.Fatalf("bob reads %s = %+v, %v; want order.not_found", id, out, err)
			}
			if rs.byIDForCustomer.Load() != 1 || rs.byID.Load() != 0 {
				t.Errorf("bob reads %s: byIDForCustomer=%d byID=%d, want 1/0 (the owner condition belongs in the query)", id, rs.byIDForCustomer.Load(), rs.byID.Load())
			}
			if ls.lists.Load() != 0 {
				t.Errorf("bob reads %s: the history table was read %d time(s) before ownership was decided", id, ls.lists.Load())
			}
		}

		// An admin and an anonymous caller (auth off) are not confined; a missing order is still NOT_FOUND for them.
		for name, ctx := range map[string]context.Context{"admin": asAdmin("ops"), "anonymous": noPrincipal()} {
			reset()
			if got := readHistory(t, ctx, uc, "o-alice"); len(got.Entries) != 1 {
				t.Errorf("%s reads alice's history = %+v, want 1 entry", name, got.Entries)
			}
			if rs.byID.Load() != 1 || rs.byIDForCustomer.Load() != 0 {
				t.Errorf("%s read: byID=%d byIDForCustomer=%d, want 1/0", name, rs.byID.Load(), rs.byIDForCustomer.Load())
			}
			if _, err := uc.Execute(ctx, dto.GetOrderHistoryInput{OrderID: "o-never"}); !errors.Is(err, order.ErrNotFound) {
				t.Errorf("%s reads a missing order: %v, want order.not_found", name, err)
			}
		}

		if _, err := uc.Execute(asAdmin("ops"), dto.GetOrderHistoryInput{}); !errors.Is(err, order.ErrInvalidID) {
			t.Errorf("empty id: %v, want order.invalid_id", err)
		}
	})
}

// An order that predates the history table has no rows: its owner and an admin get an empty list, not
// an error. (Control: the same call on a missing order is NOT_FOUND, above.)
func TestGetOrderHistory_OrderWithoutHistoryIsAnEmptyList(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		seed, err := order.Reconstitute(order.ReconstituteParams{
			ID: "o-legacy", CustomerID: "alice", Status: order.StatusConfirmed, CreatedAt: day(8), UpdatedAt: day(8),
			Items: legacyItems(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.repo.Save(context.Background(), seed); err != nil {
			t.Fatal(err)
		}
		if rows := s.rows("o-legacy"); len(rows) != 0 {
			t.Fatalf("fixture has history rows: %v", rows)
		}

		uc := s.historyUC()
		for name, ctx := range map[string]context.Context{"owner": asCustomer("alice"), "admin": asAdmin("ops"), "anonymous": noPrincipal()} {
			got := readHistory(t, ctx, uc, "o-legacy")
			if got.Entries == nil || len(got.Entries) != 0 {
				t.Errorf("%s: entries = %v (nil=%v), want an empty, non-nil list", name, got.Entries, got.Entries == nil)
			}
		}
	})
}

// actor_subject is shown to an admin only. The rows are written by real use cases with real
// principals, so the subject column is non-empty (positive control) and each caller below is
// compared with what the admin sees for the very same rows.
func TestGetOrderHistory_ActorSubjectIsAdminOnly(t *testing.T) {
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(10))
		s.newOrder(asCustomer("alice"), "o-1", "alice") // CUSTOMER alice
		s.doConfirm("o-1")                              // SYSTEM
		s.clock.set(day(11))
		if out, err := s.complete.Execute(asAdmin("ops"), dto.CompleteOrderInput{OrderID: "o-1"}); err != nil || out.Status != "COMPLETED" { // ADMIN ops
			t.Fatalf("complete = %+v, %v", out, err)
		}

		// Anchor: what is stored. The masking assertions below are meaningless if these were empty.
		stored := s.rows("o-1")
		if len(stored) != 3 || stored[0].Subject != "alice" || stored[1].Subject != "" || stored[2].Subject != "ops" {
			t.Fatalf("stored rows = %+v, want subjects alice / (system) / ops", stored)
		}

		uc := s.historyUC()
		type view struct{ kind, subject string }
		views := func(ctx context.Context) []view {
			var out []view
			for _, e := range readHistory(t, ctx, uc, "o-1").Entries {
				out = append(out, view{e.ActorKind, e.ActorSubject})
			}

			return out
		}

		adminView := views(asAdmin("another-admin"))
		wantAdmin := []view{{"CUSTOMER", "alice"}, {"SYSTEM", ""}, {"ADMIN", "ops"}}
		if len(adminView) != 3 || adminView[0] != wantAdmin[0] || adminView[1] != wantAdmin[1] || adminView[2] != wantAdmin[2] {
			t.Fatalf("admin sees %v, want %v", adminView, wantAdmin)
		}

		// Everybody else sees the same kinds and never a subject: the owner, and anonymous (auth off).
		wantOthers := []view{{"CUSTOMER", ""}, {"SYSTEM", ""}, {"ADMIN", ""}}
		for name, ctx := range map[string]context.Context{"owner": asCustomer("alice"), "anonymous": noPrincipal()} {
			got := views(ctx)
			if len(got) != 3 || got[0] != wantOthers[0] || got[1] != wantOthers[1] || got[2] != wantOthers[2] {
				t.Errorf("%s sees %v, want %v (kinds kept, subjects hidden)", name, got, wantOthers)
			}
		}
	})
}

// ---------------------------------------------------------------------------------------------
// OrderCancellationRequested.cause: a known value is recorded as is; a missing one (an event
// published before the field existed) and an unknown one are an unclassified request.
// ---------------------------------------------------------------------------------------------

// hasField reports whether the wire bytes carry field num.
func hasField(t *testing.T, b []byte, num protowire.Number) bool {
	t.Helper()
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			t.Fatalf("bad tag: %v", protowire.ParseError(l))
		}
		b = b[l:]
		if n == num {
			return true
		}
		l = protowire.ConsumeFieldValue(n, typ, b)
		if l < 0 {
			t.Fatalf("bad field %d: %v", n, protowire.ParseError(l))
		}
		b = b[l:]
	}

	return false
}

func TestCancellationRequested_CauseIsRecordedOrUnclassified(t *testing.T) {
	cases := []struct {
		name      string
		cause     orderingv1.CancellationCause
		wantField bool // the bytes carry field 4
		want      string
	}{
		{"stock shortage", orderingv1.CancellationCause_CANCELLATION_CAUSE_STOCK_SHORTAGE, true, "STOCK_SHORTAGE"},
		{"reservation expired", orderingv1.CancellationCause_CANCELLATION_CAUSE_RESERVATION_EXPIRED, true, "RESERVATION_EXPIRED"},
		{"event from before the field existed", orderingv1.CancellationCause_CANCELLATION_CAUSE_UNSPECIFIED, false, "CANCELLATION_REQUESTED"},
		{"a value this version does not know", orderingv1.CancellationCause(99), true, "CANCELLATION_REQUESTED"},
	}
	backends(t, func(t *testing.T, mk func(stackOpts) *hstack) {
		s := mk(stackOpts{})
		s.clock.set(day(10))
		handler := consumer.NewOrderCancellationHandler(s.cancel)

		for i, tc := range cases {
			id := "o-cause-" + string(rune('a'+i))
			s.newOrder(asCustomer("alice"), id, "alice")
			s.doConfirm(id)
			before := s.rows(id)
			if len(before) != 2 || before[1].Event != "CONFIRMED" {
				t.Fatalf("%s: fixture rows = %+v, want CREATED, CONFIRMED", tc.name, before)
			}

			payload, err := proto.Marshal(&orderingv1.OrderCancellationRequested{
				OrderId:     id,
				Reason:      "inventory: insufficient stock for SKU-SECRET", // free text: never a cause
				RequestedAt: timestamppb.New(day(10)),
				Cause:       tc.cause,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := hasField(t, payload, 4); got != tc.wantField {
				t.Fatalf("%s: payload carries field 4 = %v, want %v (the fixture is not the shape it claims)", tc.name, got, tc.wantField)
			}

			if err := handler.Handle(noPrincipal(), payload); err != nil {
				t.Errorf("%s: Handle = %v", tc.name, err)

				continue
			}
			s.expectRows(id, before[0], before[1], want(3, "CANCELLED", "CONFIRMED", "CANCELLED", day(10), "SYSTEM", "", tc.want))
		}
	})
}
