// Package rpc adapts the generated identity.v1 contract to use cases.
package rpc

import (
	"context"
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
	CreateAccountUC  = usecasex.UseCase[dto.CreateAccountInput, *dto.AccountOutput]
	GetAccountUC     = usecasex.UseCase[dto.GetAccountInput, *dto.AccountOutput]
	ListAccountsUC   = usecasex.UseCase[dto.ListAccountsInput, *dto.ListAccountsOutput]
	DisableAccountUC = usecasex.UseCase[dto.DisableAccountInput, *dto.AccountOutput]
	EnableAccountUC  = usecasex.UseCase[dto.EnableAccountInput, *dto.AccountOutput]
	AssignRolesUC    = usecasex.UseCase[dto.AssignRolesInput, *dto.AccountOutput]
	CreateAPIKeyUC   = usecasex.UseCase[dto.CreateAPIKeyInput, *dto.CreateAPIKeyOutput]
	ListAPIKeysUC    = usecasex.UseCase[dto.ListAPIKeysInput, *dto.ListAPIKeysOutput]
	RevokeAPIKeyUC   = usecasex.UseCase[dto.RevokeAPIKeyInput, *dto.APIKeyOutput]
	WhoAmIUC         = usecasex.UseCase[dto.WhoAmIInput, *dto.WhoAmIOutput]
)

// UseCases groups what the service needs; wire fills it by field.
type UseCases struct {
	CreateAccount  CreateAccountUC
	GetAccount     GetAccountUC
	ListAccounts   ListAccountsUC
	DisableAccount DisableAccountUC
	EnableAccount  EnableAccountUC
	AssignRoles    AssignRolesUC
	CreateAPIKey   CreateAPIKeyUC
	ListAPIKeys    ListAPIKeysUC
	RevokeAPIKey   RevokeAPIKeyUC
	WhoAmI         WhoAmIUC
}

// IdentityService implements the generated IdentityServiceHandler. Every
// procedure but WhoAmI is behind server.RequireRole(auth.AdminRole): managing
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
	return &identityv1.Account{
		Id: a.ID, Name: a.Name, Roles: a.Roles, Status: toProtoAccountStatus(a.Status),
		CreateTime: timestamppb.New(a.CreatedAt), UpdateTime: timestamppb.New(a.UpdatedAt),
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
