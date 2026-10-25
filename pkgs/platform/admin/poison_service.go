// Package admin is the operator surface of the platform, served by apps/rpc
// next to the bounded contexts' services: admin.v1.AdminService over the
// poison queues of every BC the server hosts (platform/eventbus). It is not a
// BC — it has no domain — so it is a thin handler over the eventbus
// functions, guarded by server.RequireRole.
package admin

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

const defaultLimit = 50

var _ adminv1connect.AdminServiceHandler = (*PoisonService)(nil)

// PoisonService serves AdminService. schemas are the BCs whose poison tables
// exist (apps/rpc lists them); a nil db (no database configured) answers
// every call with FailedPrecondition.
type PoisonService struct {
	db      *gorm.DB
	schemas []string
	auth    config.Auth
}

func NewPoisonService(db *gorm.DB, auth config.Auth, schemas []string) *PoisonService {
	// A config built by hand (tests) skips config.Load's defaults; an empty
	// role would deny everyone with auth on.
	if auth.AdminRole == "" {
		auth.AdminRole = DefaultAdminRole
	}

	return &PoisonService{db: db, schemas: schemas, auth: auth}
}

// DefaultAdminRole is auth.admin_role when unset (config.Load sets the same).
const DefaultAdminRole = "admin"

// Route mounts the service (Connect + gRPC + gRPC-Web on one port).
func (h *PoisonService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return adminv1connect.NewAdminServiceHandler(h, opts...)
}

func (h *PoisonService) ListPoison(c context.Context, req *connect.Request[adminv1.ListPoisonRequest]) (*connect.Response[adminv1.ListPoisonResponse], error) {
	if err := h.guard(c); err != nil {
		return nil, err
	}
	bcs := h.schemas
	if bc := req.Msg.GetBoundedContext(); bc != "" {
		if err := h.known(bc); err != nil {
			return nil, err
		}
		bcs = []string{bc}
	}
	limit := int(req.Msg.GetLimit())
	if limit == 0 {
		limit = defaultLimit
	}
	res := &adminv1.ListPoisonResponse{}
	for _, bc := range bcs {
		rows, err := eventbus.ListPoison(c, h.db, eventbus.PoisonTopic(bc), limit)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, m := range rows {
			res.Messages = append(res.Messages, toProto(bc, m, false))
		}
	}

	return connect.NewResponse(res), nil
}

func (h *PoisonService) GetPoison(c context.Context, req *connect.Request[adminv1.GetPoisonRequest]) (*connect.Response[adminv1.GetPoisonResponse], error) {
	if err := h.guard(c); err != nil {
		return nil, err
	}
	bc := req.Msg.GetBoundedContext()
	if err := h.known(bc); err != nil {
		return nil, err
	}
	rows, err := eventbus.ListPoison(c, h.db, eventbus.PoisonTopic(bc), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, m := range rows {
		if m.Offset == req.Msg.GetOffset() {
			return connect.NewResponse(&adminv1.GetPoisonResponse{Message: toProto(bc, m, true)}), nil
		}
	}
	ce := connect.NewError(connect.CodeNotFound, errNoRow)
	ce.Meta().Set("x-error-code", "admin.poison_not_found")

	return nil, ce
}

func (h *PoisonService) RequeuePoison(c context.Context, req *connect.Request[adminv1.RequeuePoisonRequest]) (*connect.Response[adminv1.RequeuePoisonResponse], error) {
	n, err := h.act(c, req.Msg.GetBoundedContext(), req.Msg.GetOffsets(), eventbus.Requeue)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&adminv1.RequeuePoisonResponse{Count: n}), nil
}

func (h *PoisonService) DropPoison(c context.Context, req *connect.Request[adminv1.DropPoisonRequest]) (*connect.Response[adminv1.DropPoisonResponse], error) {
	n, err := h.act(c, req.Msg.GetBoundedContext(), req.Msg.GetOffsets(), eventbus.Drop)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&adminv1.DropPoisonResponse{Count: n}), nil
}

// Prune is `worker prune` over RPC (eventbus.PruneSchema per BC). Every
// hosted BC's poison table protects the inbox rows, whichever BC is pruned.
// No transaction here on purpose: each DELETE batch commits on its own.
func (h *PoisonService) Prune(c context.Context, req *connect.Request[adminv1.PruneRequest]) (*connect.Response[adminv1.PruneResponse], error) {
	if err := h.guard(c); err != nil {
		return nil, err
	}
	bcs := h.schemas
	if bc := req.Msg.GetBoundedContext(); bc != "" {
		if err := h.known(bc); err != nil {
			return nil, err
		}
		bcs = []string{bc}
	}
	opts := eventbus.PruneOptions{
		OlderThan:    req.Msg.GetOlderThan().AsDuration(),
		Unconsumed:   req.Msg.GetUnconsumed(),
		DryRun:       req.Msg.GetDryRun(),
		BatchSize:    int(req.Msg.GetBatchSize()),
		ForgetGroups: req.Msg.GetForgetGroups(),
	}
	res := &adminv1.PruneResponse{}
	for _, bc := range bcs {
		results, err := eventbus.PruneSchema(c, h.db, bc, opts, h.schemas)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, r := range results {
			res.Total += r.Rows
			res.Tables = append(res.Tables, pruneToProto(r))
		}
	}

	return connect.NewResponse(res), nil
}

func pruneToProto(r eventbus.PruneResult) *adminv1.PruneTable {
	out := &adminv1.PruneTable{Table: r.Table, Rows: r.Rows, Skipped: r.Skipped, Forgotten: r.Forgotten}
	for _, b := range r.Blockers {
		out.Blockers = append(out.Blockers, &adminv1.PruneBlocker{ConsumerGroup: b.Group, Unread: b.Unread, OldestUnread: durationpb.New(b.OldestUnread)})
	}

	return out
}

// act runs op on the BC's rows (offsets empty = all, the request's xor rule
// already made sure "all" was explicit).
func (h *PoisonService) act(c context.Context, bc string, offsets []int64, op func(context.Context, *gorm.DB, string, ...int64) (int, error)) (int32, error) {
	if err := h.guard(c); err != nil {
		return 0, err
	}
	if err := h.known(bc); err != nil {
		return 0, err
	}
	n, err := op(c, h.db, eventbus.PoisonTopic(bc), offsets...)
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err)
	}

	return int32(n), nil //nolint:gosec // row counts
}

// guard is the role check plus the "is there a database" check.
func (h *PoisonService) guard(c context.Context) error {
	if err := server.RequireRole(c, h.auth, h.auth.AdminRole); err != nil {
		return err
	}
	if h.db == nil {
		ce := connect.NewError(connect.CodeFailedPrecondition, errNoDB)
		ce.Meta().Set("x-error-code", "admin.no_database")
		return ce
	}

	return nil
}

func (h *PoisonService) known(bc string) error {
	for _, s := range h.schemas {
		if s == bc {
			return nil
		}
	}
	ce := connect.NewError(connect.CodeInvalidArgument, errUnknownBC)
	ce.Meta().Set("x-error-code", "admin.unknown_bounded_context")

	return ce
}

func toProto(bc string, m eventbus.PoisonMessage, withPayload bool) *adminv1.PoisonMessage {
	out := &adminv1.PoisonMessage{
		BoundedContext: bc,
		Offset:         m.Offset,
		Uuid:           m.UUID,
		ParkedAt:       timestamppb.New(m.CreatedAt),
		Topic:          m.Topic,
		Handler:        m.Handler,
		Reason:         m.Reason,
		Metadata:       map[string]string(m.Metadata),
	}
	if withPayload {
		out.Payload = m.Payload
	}

	return out
}
