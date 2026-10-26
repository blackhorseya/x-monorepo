package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// WhoAmI reports the principal the auth interceptor put in the context: no
// repository, no role — a client's way to check what its credential is worth.
type WhoAmI struct{}

func NewWhoAmI() *WhoAmI { return &WhoAmI{} }

func (uc *WhoAmI) Execute(c context.Context, _ dto.WhoAmIInput) (*dto.WhoAmIOutput, error) {
	p := contextx.PrincipalFrom(c)
	if p.IsAnonymous() {
		return &dto.WhoAmIOutput{Anonymous: true}, nil
	}

	return &dto.WhoAmIOutput{Subject: p.Subject, Name: p.Name, Roles: p.Roles, Method: p.Method, CredentialID: p.CredentialID}, nil
}
