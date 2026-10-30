package auth

import (
	"context"
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// ExternalCredentials adapts ExternalResolver to server.ExternalIdentityResolver.
// RED stub.
type ExternalCredentials struct{ r *ExternalResolver }

var _ server.ExternalIdentityResolver = (*ExternalCredentials)(nil)

func NewExternalCredentials(r *ExternalResolver) *ExternalCredentials {
	return &ExternalCredentials{r: r}
}

func (x *ExternalCredentials) ResolveExternal(context.Context, contextx.ExternalIdentity) (contextx.Principal, error) {
	return contextx.Principal{}, errors.New("not implemented")
}

func (x *ExternalCredentials) AllowsUnbound(string) bool { return false }
