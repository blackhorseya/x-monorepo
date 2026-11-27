package server

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// interceptor enforces the declarations. It sits inside auth, whose principal
// and external identity it reads, and outside the timeout and validation, so a
// refused caller learns nothing about the request rules. It is mounted only
// when auth is.
func (r *authzRegistry) interceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			decl, err := declarationOf(spec)
			if err != nil {
				// New checked every mounted procedure; refuse rather than guess.
				contextx.FromContext(c).Error("authz: unreadable declaration", "procedure", spec.Procedure, "error", err)

				return denied()
			}
			if decl.GetPublic() {
				return next(c, spec, stream)
			}
			p := contextx.PrincipalFrom(c)
			if _, external := contextx.ExternalIdentityFrom(c); p.IsAnonymous() && !external {
				return authError(connect.CodeUnauthenticated, UnauthenticatedCode, errors.New("credentials required"))
			}
			var kind string
			if r.classify != nil {
				kind = r.classify(c)
			}
			if _, registered := r.kinds[kind]; kind != "" && !registered {
				contextx.FromContext(c).Error("authz: the classifier answered an unregistered principal kind", "procedure", spec.Procedure, "kind", kind)

				return denied()
			}
			if !r.admits(decl.GetRules().GetAnyOf(), p, kind) {
				return denied()
			}
			if kind != "" {
				c = contextx.WithPrincipalKind(c, kind)
			}

			return next(c, spec, stream)
		}
	}
}

// admits reports whether the caller meets one of rules. A permission is held by
// a principal only: an identity with no account holds none, and meets a rule
// that names its kind alone.
func (r *authzRegistry) admits(rules []*commonv1.AuthzRule, p contextx.Principal, kind string) bool {
	for _, rule := range rules {
		want, permission := rule.GetKind(), rule.GetPermission()
		if want != "" && want != kind {
			continue
		}
		if permission == "" {
			if want != "" {
				return true
			}

			continue
		}
		if role, ok := r.roles[permission]; ok && !p.IsAnonymous() && p.HasRole(role) {
			return true
		}
	}

	return false
}

func denied() error {
	return authError(connect.CodePermissionDenied, PermissionDeniedCode, errors.New("permission denied"))
}
