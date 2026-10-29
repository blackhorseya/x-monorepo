package auth

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// ResolveExternalUC and ProvisionExternalUC are the use cases behind ExternalResolver.
type (
	ResolveExternalUC   = usecasex.UseCase[dto.ResolveExternalInput, *dto.ResolveExternalOutput]
	ProvisionExternalUC = usecasex.UseCase[dto.ProvisionExternalInput, *dto.AccountOutput]
)

// ExternalIdentity is what the resolver needs of a verified OIDC token. It is
// a plain value, defined here at the consumer: whoever verifies the token
// (the platform's OIDC verifier, wired by the auth interceptor) fills it.
type ExternalIdentity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	IssuedAt      time.Time // the token's iat; zero when the token had none
}

// ExternalPrincipal is the account a verified token speaks for.
type ExternalPrincipal struct {
	AccountID         string
	Name              string
	Roles             []string
	SessionsRevokedAt time.Time // zero: never revoked
}

// StaticMapping is an IssuerMapping from a fixed table; an issuer not in it is bound.
type StaticMapping map[string]externalidentity.Mapping

var _ port.IssuerMapping = StaticMapping(nil)

func (m StaticMapping) MappingFor(issuer string) externalidentity.Mapping {
	if v, ok := m[issuer]; ok {
		return v
	}

	return externalidentity.MappingBound
}

// ExternalResolver maps a verified (issuer, subject) to an account (RED stub).
type ExternalResolver struct{}

// NewExternalResolver builds the resolver with DefaultCacheTTL.
func NewExternalResolver(resolve ResolveExternalUC, provision ProvisionExternalUC, mapping port.IssuerMapping, clock port.Clock, mp metric.MeterProvider) (*ExternalResolver, error) {
	return &ExternalResolver{}, nil
}

// ResolveExternal returns the principal, or an error that refuses the token.
func (r *ExternalResolver) ResolveExternal(c context.Context, id ExternalIdentity) (ExternalPrincipal, error) {
	return ExternalPrincipal{}, errors.New("not implemented")
}
