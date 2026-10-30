// Package rpc adapts the generated identity.v1 contract to use cases.
package rpc

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ identityv1connect.IdentityServiceHandler = (*IdentityService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	CreateAccountUC         = usecasex.UseCase[dto.CreateAccountInput, *dto.AccountOutput]
	GetAccountUC            = usecasex.UseCase[dto.GetAccountInput, *dto.AccountOutput]
	ListAccountsUC          = usecasex.UseCase[dto.ListAccountsInput, *dto.ListAccountsOutput]
	DisableAccountUC        = usecasex.UseCase[dto.DisableAccountInput, *dto.AccountOutput]
	EnableAccountUC         = usecasex.UseCase[dto.EnableAccountInput, *dto.AccountOutput]
	AssignRolesUC           = usecasex.UseCase[dto.AssignRolesInput, *dto.AccountOutput]
	CreateAPIKeyUC          = usecasex.UseCase[dto.CreateAPIKeyInput, *dto.CreateAPIKeyOutput]
	ListAPIKeysUC           = usecasex.UseCase[dto.ListAPIKeysInput, *dto.ListAPIKeysOutput]
	RevokeAPIKeyUC          = usecasex.UseCase[dto.RevokeAPIKeyInput, *dto.APIKeyOutput]
	WhoAmIUC                = usecasex.UseCase[dto.WhoAmIInput, *dto.WhoAmIOutput]
	CreateInviteUC          = usecasex.UseCase[dto.CreateInviteInput, *dto.InviteOutput]
	ClaimInviteUC           = usecasex.UseCase[dto.ClaimInviteInput, *dto.AccountOutput]
	ListAccountIdentitiesUC = usecasex.UseCase[dto.ListAccountIdentitiesInput, *dto.ListAccountIdentitiesOutput]
	RevokeSessionsUC        = usecasex.UseCase[dto.RevokeSessionsInput, *dto.AccountOutput]
)

// ExternalIdentity is the verified external identity of one call: what the
// token said, after the server verified its signature and claims. It is a
// plain value defined here, at the consumer; the handler never trusts a client
// to supply it.
type ExternalIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
}

// ExternalIdentitySource hands the handler the verified external identity of
// the call, if the request carried one. The auth interceptor (#272) provides
// the real one; until then NoExternalIdentity answers "none" and ClaimInvite
// is UNAUTHENTICATED (fail-closed).
type ExternalIdentitySource interface {
	ExternalIdentityFrom(c context.Context) (ExternalIdentity, bool)
}

// NoExternalIdentity is the source of a server with OIDC off: nobody has one.
type NoExternalIdentity struct{}

func (NoExternalIdentity) ExternalIdentityFrom(context.Context) (ExternalIdentity, bool) {
	return ExternalIdentity{}, false
}

// UseCases groups what the service needs; wire fills it by field.
type UseCases struct {
	CreateAccount         CreateAccountUC
	GetAccount            GetAccountUC
	ListAccounts          ListAccountsUC
	DisableAccount        DisableAccountUC
	EnableAccount         EnableAccountUC
	AssignRoles           AssignRolesUC
	CreateAPIKey          CreateAPIKeyUC
	ListAPIKeys           ListAPIKeysUC
	RevokeAPIKey          RevokeAPIKeyUC
	WhoAmI                WhoAmIUC
	CreateInvite          CreateInviteUC
	RevokeSessions        RevokeSessionsUC
	ClaimInvite           ClaimInviteUC
	ListAccountIdentities ListAccountIdentitiesUC
	// ExternalIdentity is where ClaimInvite learns who is calling.
	ExternalIdentity ExternalIdentitySource
}

// IdentityService implements the generated IdentityServiceHandler. Every
// procedure but WhoAmI and ClaimInvite is behind server.RequireRole(auth.AdminRole): managing
// who may call is the operator's job.
type IdentityService struct {
	uc   UseCases
	auth config.Auth
}

func NewIdentityService(uc UseCases, auth config.Auth) *IdentityService {
	if auth.AdminRole == "" {
		auth.AdminRole = admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return &IdentityService{uc: uc, auth: auth}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *IdentityService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return identityv1connect.NewIdentityServiceHandler(h, opts...)
}

func (h *IdentityService) admin(c context.Context) error {
	return server.RequireRole(c, h.auth, h.auth.AdminRole)
}

func (h *IdentityService) CreateAccount(c context.Context, req *connect.Request[identityv1.CreateAccountRequest]) (*connect.Response[identityv1.CreateAccountResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.CreateAccount.Execute(c, dto.CreateAccountInput{Name: req.Msg.GetName(), Roles: req.Msg.GetRoles()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.CreateAccountResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) GetAccount(c context.Context, req *connect.Request[identityv1.GetAccountRequest]) (*connect.Response[identityv1.GetAccountResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.GetAccount.Execute(c, dto.GetAccountInput{ID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.GetAccountResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) ListAccounts(c context.Context, req *connect.Request[identityv1.ListAccountsRequest]) (*connect.Response[identityv1.ListAccountsResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAccounts.Execute(c, dto.ListAccountsInput{PageSize: req.Msg.GetPageSize(), PageToken: req.Msg.GetPageToken()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	accounts := make([]*identityv1.Account, 0, len(out.Accounts))
	for i := range out.Accounts {
		accounts = append(accounts, toProtoAccount(&out.Accounts[i]))
	}

	return connect.NewResponse(&identityv1.ListAccountsResponse{Accounts: accounts, NextPageToken: out.NextPageToken}), nil
}

func (h *IdentityService) DisableAccount(c context.Context, req *connect.Request[identityv1.DisableAccountRequest]) (*connect.Response[identityv1.DisableAccountResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.DisableAccount.Execute(c, dto.DisableAccountInput{ID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.DisableAccountResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) EnableAccount(c context.Context, req *connect.Request[identityv1.EnableAccountRequest]) (*connect.Response[identityv1.EnableAccountResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.EnableAccount.Execute(c, dto.EnableAccountInput{ID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.EnableAccountResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) AssignRoles(c context.Context, req *connect.Request[identityv1.AssignRolesRequest]) (*connect.Response[identityv1.AssignRolesResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.AssignRoles.Execute(c, dto.AssignRolesInput{ID: req.Msg.GetId(), Roles: req.Msg.GetRoles()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.AssignRolesResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) CreateApiKey(c context.Context, req *connect.Request[identityv1.CreateApiKeyRequest]) (*connect.Response[identityv1.CreateApiKeyResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	in := dto.CreateAPIKeyInput{AccountID: req.Msg.GetAccountId(), Name: req.Msg.GetName()}
	if req.Msg.GetExpireTime() != nil {
		in.ExpiresAt = req.Msg.GetExpireTime().AsTime()
	}
	out, err := h.uc.CreateAPIKey.Execute(c, in)
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.CreateApiKeyResponse{ApiKey: toProtoAPIKey(&out.Key), Secret: out.Secret}), nil
}

func (h *IdentityService) ListApiKeys(c context.Context, req *connect.Request[identityv1.ListApiKeysRequest]) (*connect.Response[identityv1.ListApiKeysResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAPIKeys.Execute(c, dto.ListAPIKeysInput{AccountID: req.Msg.GetAccountId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	keys := make([]*identityv1.ApiKey, 0, len(out.Keys))
	for i := range out.Keys {
		keys = append(keys, toProtoAPIKey(&out.Keys[i]))
	}

	return connect.NewResponse(&identityv1.ListApiKeysResponse{ApiKeys: keys}), nil
}

func (h *IdentityService) RevokeApiKey(c context.Context, req *connect.Request[identityv1.RevokeApiKeyRequest]) (*connect.Response[identityv1.RevokeApiKeyResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.RevokeAPIKey.Execute(c, dto.RevokeAPIKeyInput{ID: req.Msg.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.RevokeApiKeyResponse{ApiKey: toProtoAPIKey(out)}), nil
}

func (h *IdentityService) CreateInvite(c context.Context, req *connect.Request[identityv1.CreateInviteRequest]) (*connect.Response[identityv1.CreateInviteResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.CreateInvite.Execute(c, dto.CreateInviteInput{AccountID: req.Msg.GetAccountId(), Email: req.Msg.GetEmail()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.CreateInviteResponse{Invite: toProtoInvite(out)}), nil
}

func (h *IdentityService) ListAccountIdentities(c context.Context, req *connect.Request[identityv1.ListAccountIdentitiesRequest]) (*connect.Response[identityv1.ListAccountIdentitiesResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAccountIdentities.Execute(c, dto.ListAccountIdentitiesInput{AccountID: req.Msg.GetAccountId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	identities := make([]*identityv1.ExternalIdentity, 0, len(out.Identities))
	for _, e := range out.Identities {
		identities = append(identities, &identityv1.ExternalIdentity{
			Issuer: e.Issuer, Subject: e.Subject, AccountId: e.AccountID, BindTime: timestamppb.New(e.BoundAt),
		})
	}
	invites := make([]*identityv1.Invite, 0, len(out.Invites))
	for i := range out.Invites {
		invites = append(invites, toProtoInvite(&out.Invites[i]))
	}

	return connect.NewResponse(&identityv1.ListAccountIdentitiesResponse{Identities: identities, Invites: invites}), nil
}

// RevokeSessions is "sign out of all devices", not a compromise response; the
// contract comment on the RPC says so, and so must any UI that offers it.
func (h *IdentityService) RevokeSessions(c context.Context, req *connect.Request[identityv1.RevokeSessionsRequest]) (*connect.Response[identityv1.RevokeSessionsResponse], error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.RevokeSessions.Execute(c, dto.RevokeSessionsInput{AccountID: req.Msg.GetAccountId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.RevokeSessionsResponse{Account: toProtoAccount(out)}), nil
}

// ClaimInvite binds the caller's verified external identity through the
// invite for its email. Not admin-only: it is what an unbound identity calls.
func (h *IdentityService) ClaimInvite(c context.Context, _ *connect.Request[identityv1.ClaimInviteRequest]) (*connect.Response[identityv1.ClaimInviteResponse], error) {
	var (
		id ExternalIdentity
		ok bool
	)
	if h.uc.ExternalIdentity != nil {
		id, ok = h.uc.ExternalIdentity.ExternalIdentityFrom(c)
	}
	if !ok {
		ce := connect.NewError(connect.CodeUnauthenticated, errors.New("a verified external identity is required"))
		ce.Meta().Set("x-error-code", server.UnauthenticatedCode)

		return nil, ce
	}
	out, err := h.uc.ClaimInvite.Execute(c, dto.ClaimInviteInput{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.ClaimInviteResponse{Account: toProtoAccount(out)}), nil
}

func (h *IdentityService) WhoAmI(c context.Context, _ *connect.Request[identityv1.WhoAmIRequest]) (*connect.Response[identityv1.WhoAmIResponse], error) {
	out, err := h.uc.WhoAmI.Execute(c, dto.WhoAmIInput{})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&identityv1.WhoAmIResponse{
		Anonymous: out.Anonymous, Subject: out.Subject, Name: out.Name, Roles: out.Roles, Method: out.Method, CredentialId: out.CredentialID,
	}), nil
}

func toProtoAccount(a *dto.AccountOutput) *identityv1.Account {
	out := &identityv1.Account{
		Id: a.ID, Name: a.Name, Roles: a.Roles, Status: toProtoAccountStatus(a.Status),
		CreateTime: timestamppb.New(a.CreatedAt), UpdateTime: timestamppb.New(a.UpdatedAt),
	}
	if !a.SessionsRevokedAt.IsZero() {
		out.SessionsRevokeTime = timestamppb.New(a.SessionsRevokedAt)
	}

	return out
}

func toProtoInvite(v *dto.InviteOutput) *identityv1.Invite {
	out := &identityv1.Invite{
		Id: v.ID, Email: v.Email, AccountId: v.AccountID,
		CreateTime: timestamppb.New(v.CreatedAt), ExpireTime: timestamppb.New(v.ExpiresAt),
		Status: toProtoInviteStatus(v.Status),
	}
	if !v.ClaimedAt.IsZero() {
		out.ClaimTime = timestamppb.New(v.ClaimedAt)
	}

	return out
}

func toProtoInviteStatus(s string) identityv1.InviteStatus {
	switch s {
	case "PENDING":
		return identityv1.InviteStatus_INVITE_STATUS_PENDING
	case "CLAIMED":
		return identityv1.InviteStatus_INVITE_STATUS_CLAIMED
	case "EXPIRED":
		return identityv1.InviteStatus_INVITE_STATUS_EXPIRED
	default:
		return identityv1.InviteStatus_INVITE_STATUS_UNSPECIFIED
	}
}

func toProtoAccountStatus(s string) identityv1.AccountStatus {
	switch s {
	case "ACTIVE":
		return identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	case "DISABLED":
		return identityv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	default:
		return identityv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED
	}
}

func toProtoAPIKey(k *dto.APIKeyOutput) *identityv1.ApiKey {
	out := &identityv1.ApiKey{
		Id: k.ID, AccountId: k.AccountID, Name: k.Name, Prefix: k.Prefix, Status: toProtoAPIKeyStatus(k.Status),
		CreateTime: timestamppb.New(k.CreatedAt),
	}
	if !k.ExpiresAt.IsZero() {
		out.ExpireTime = timestamppb.New(k.ExpiresAt)
	}
	if !k.RevokedAt.IsZero() {
		out.RevokeTime = timestamppb.New(k.RevokedAt)
	}
	if !k.LastUsedAt.IsZero() {
		out.LastUseTime = timestamppb.New(k.LastUsedAt)
	}

	return out
}

func toProtoAPIKeyStatus(s string) identityv1.ApiKeyStatus {
	switch s {
	case "ACTIVE":
		return identityv1.ApiKeyStatus_API_KEY_STATUS_ACTIVE
	case "REVOKED":
		return identityv1.ApiKeyStatus_API_KEY_STATUS_REVOKED
	default:
		return identityv1.ApiKeyStatus_API_KEY_STATUS_UNSPECIFIED
	}
}
