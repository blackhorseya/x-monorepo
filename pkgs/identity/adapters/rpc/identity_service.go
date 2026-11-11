// Package rpc adapts the generated identity.v1 contract to use cases.
package rpc

import (
	"context"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
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
// the call, if the request carried one: ContextExternalIdentity reads what the
// auth interceptor verified. Without one ClaimInvite is UNAUTHENTICATED
// (fail-closed); NoExternalIdentity is the source for a caller that has none.
type ExternalIdentitySource interface {
	ExternalIdentityFrom(c context.Context) (ExternalIdentity, bool)
}

// NoExternalIdentity is the source that never has one (hand-built services in tests).
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

// Register serves the service on s (Connect + gRPC + gRPC-Web on one port).
func (h *IdentityService) Register(s *connect.Server) {
	identityv1connect.RegisterIdentityServiceHandler(s, h)
}

func (h *IdentityService) admin(c context.Context) error {
	return server.RequireRole(c, h.auth, h.auth.AdminRole)
}

func (h *IdentityService) CreateAccount(c context.Context, req *identityv1.CreateAccountRequest) (*identityv1.CreateAccountResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.CreateAccount.Execute(c, dto.CreateAccountInput{Name: req.GetName(), Roles: req.GetRoles()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.CreateAccountResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) GetAccount(c context.Context, req *identityv1.GetAccountRequest) (*identityv1.GetAccountResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.GetAccount.Execute(c, dto.GetAccountInput{ID: req.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.GetAccountResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) ListAccounts(c context.Context, req *identityv1.ListAccountsRequest) (*identityv1.ListAccountsResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAccounts.Execute(c, dto.ListAccountsInput{PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	accounts := make([]*identityv1.Account, 0, len(out.Accounts))
	for i := range out.Accounts {
		accounts = append(accounts, toProtoAccount(&out.Accounts[i]))
	}

	return &identityv1.ListAccountsResponse{Accounts: accounts, NextPageToken: out.NextPageToken}, nil
}

func (h *IdentityService) DisableAccount(c context.Context, req *identityv1.DisableAccountRequest) (*identityv1.DisableAccountResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.DisableAccount.Execute(c, dto.DisableAccountInput{ID: req.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.DisableAccountResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) EnableAccount(c context.Context, req *identityv1.EnableAccountRequest) (*identityv1.EnableAccountResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.EnableAccount.Execute(c, dto.EnableAccountInput{ID: req.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.EnableAccountResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) AssignRoles(c context.Context, req *identityv1.AssignRolesRequest) (*identityv1.AssignRolesResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.AssignRoles.Execute(c, dto.AssignRolesInput{ID: req.GetId(), Roles: req.GetRoles()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.AssignRolesResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) CreateApiKey(c context.Context, req *identityv1.CreateApiKeyRequest) (*identityv1.CreateApiKeyResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	in := dto.CreateAPIKeyInput{AccountID: req.GetAccountId(), Name: req.GetName()}
	if req.GetExpireTime() != nil {
		in.ExpiresAt = req.GetExpireTime().AsTime()
	}
	out, err := h.uc.CreateAPIKey.Execute(c, in)
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.CreateApiKeyResponse{ApiKey: toProtoAPIKey(&out.Key), Secret: out.Secret}, nil
}

func (h *IdentityService) ListApiKeys(c context.Context, req *identityv1.ListApiKeysRequest) (*identityv1.ListApiKeysResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAPIKeys.Execute(c, dto.ListAPIKeysInput{AccountID: req.GetAccountId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}
	keys := make([]*identityv1.ApiKey, 0, len(out.Keys))
	for i := range out.Keys {
		keys = append(keys, toProtoAPIKey(&out.Keys[i]))
	}

	return &identityv1.ListApiKeysResponse{ApiKeys: keys}, nil
}

func (h *IdentityService) RevokeApiKey(c context.Context, req *identityv1.RevokeApiKeyRequest) (*identityv1.RevokeApiKeyResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.RevokeAPIKey.Execute(c, dto.RevokeAPIKeyInput{ID: req.GetId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.RevokeApiKeyResponse{ApiKey: toProtoAPIKey(out)}, nil
}

func (h *IdentityService) CreateInvite(c context.Context, req *identityv1.CreateInviteRequest) (*identityv1.CreateInviteResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.CreateInvite.Execute(c, dto.CreateInviteInput{AccountID: req.GetAccountId(), Email: req.GetEmail()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.CreateInviteResponse{Invite: toProtoInvite(out)}, nil
}

func (h *IdentityService) ListAccountIdentities(c context.Context, req *identityv1.ListAccountIdentitiesRequest) (*identityv1.ListAccountIdentitiesResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.ListAccountIdentities.Execute(c, dto.ListAccountIdentitiesInput{AccountID: req.GetAccountId()})
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

	return &identityv1.ListAccountIdentitiesResponse{Identities: identities, Invites: invites}, nil
}

// RevokeSessions is "sign out of all devices", not a compromise response; the
// contract comment on the RPC says so, and so must any UI that offers it.
func (h *IdentityService) RevokeSessions(c context.Context, req *identityv1.RevokeSessionsRequest) (*identityv1.RevokeSessionsResponse, error) {
	if err := h.admin(c); err != nil {
		return nil, err
	}
	out, err := h.uc.RevokeSessions.Execute(c, dto.RevokeSessionsInput{AccountID: req.GetAccountId()})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.RevokeSessionsResponse{Account: toProtoAccount(out)}, nil
}

// ClaimInvite binds the caller's verified external identity through the
// invite for its email. Not admin-only: it is what an unbound identity calls.
func (h *IdentityService) ClaimInvite(c context.Context, _ *identityv1.ClaimInviteRequest) (*identityv1.ClaimInviteResponse, error) {
	var (
		id ExternalIdentity
		ok bool
	)
	if h.uc.ExternalIdentity != nil {
		id, ok = h.uc.ExternalIdentity.ExternalIdentityFrom(c)
	}
	if !ok {
		return nil, server.WithMeta(connect.NewError(connect.CodeUnauthenticated, "a verified external identity is required"), "x-error-code", server.UnauthenticatedCode)
	}
	out, err := h.uc.ClaimInvite.Execute(c, dto.ClaimInviteInput{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.ClaimInviteResponse{Account: toProtoAccount(out)}, nil
}

func (h *IdentityService) WhoAmI(c context.Context, _ *identityv1.WhoAmIRequest) (*identityv1.WhoAmIResponse, error) {
	out, err := h.uc.WhoAmI.Execute(c, dto.WhoAmIInput{})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return &identityv1.WhoAmIResponse{
		Anonymous: out.Anonymous, Subject: out.Subject, Name: out.Name, Roles: out.Roles, Method: out.Method, CredentialId: out.CredentialID,
	}, nil
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

// ContextExternalIdentity is the source of a server with OIDC on: the auth
// interceptor verified the call's bearer token and put the identity in the
// context. A call without one (API key, auth.jwt, anonymous) has none.
type ContextExternalIdentity struct{}

func (ContextExternalIdentity) ExternalIdentityFrom(c context.Context) (ExternalIdentity, bool) {
	id, ok := contextx.ExternalIdentityFrom(c)
	if !ok {
		return ExternalIdentity{}, false
	}

	return ExternalIdentity{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, EmailVerified: id.EmailVerified}, true
}
