// Package mapper converts domain objects to output DTOs (domain → dto only;
// dto → domain happens inside use cases because it involves validation).
package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
)

func ToAccountOutput(a *account.Account) *dto.AccountOutput {
	return &dto.AccountOutput{
		ID:                a.ID().String(),
		Name:              a.Name(),
		Roles:             a.Roles(),
		Status:            a.Status().String(),
		CreatedAt:         a.CreatedAt(),
		UpdatedAt:         a.UpdatedAt(),
		SessionsRevokedAt: a.SessionsRevokedAt(),
	}
}

func ToAPIKeyOutput(k *apikey.APIKey) *dto.APIKeyOutput {
	return &dto.APIKeyOutput{
		ID:         k.ID().String(),
		AccountID:  k.AccountID(),
		Name:       k.Name(),
		Prefix:     k.Prefix(),
		Status:     k.Status().String(),
		CreatedAt:  k.CreatedAt(),
		ExpiresAt:  k.ExpiresAt(),
		RevokedAt:  k.RevokedAt(),
		LastUsedAt: k.LastUsedAt(),
	}
}
