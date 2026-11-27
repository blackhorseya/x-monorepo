package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// Authorization is declared on the RPC method, in its proto (common.v1.authz):
// public, or rules of which a caller has to meet one. New refuses to start,
// whatever auth.mode is, unless every mounted procedure carries a declaration
// whose kinds and permissions are registered; the interceptor
// (authz_interceptor.go) enforces it when auth is on.

// PlatformAdminPermission is the permission of the platform's operator surface
// (admin.v1). New registers it itself, for the role auth.admin_role.
const PlatformAdminPermission = "platform.admin"

// PrincipalKind is one kind of caller a project's classifier may answer.
type PrincipalKind struct {
	Name string
	// RequiresPermission: a rule that names this kind must name a permission too.
	RequiresPermission bool
}

// kindRegistration is one WithPrincipalKinds call.
type kindRegistration struct {
	classify func(context.Context) string
	kinds    []PrincipalKind
}

// WithPrincipalKinds registers the project's kinds of caller: classify reads the
// context (contextx.PrincipalFrom, and contextx.ExternalIdentityFrom for an
// identity bound to no account) and answers one of kinds, or "" for none. It is
// called for every non-public call of an identified caller, outside
// http.timeout; an answer that is not in kinds meets no rule. A server takes at
// most one registration, and New refuses one without a classifier, without a
// kind, or with an empty or repeated name.
func WithPrincipalKinds(classify func(context.Context) string, kinds ...PrincipalKind) Option {
	return func(o *options) {
		o.kinds = append(o.kinds, kindRegistration{classify: classify, kinds: kinds})
	}
}

// WithPermissions registers permissions of the bounded contexts, each
// "<bc>.<function>"; a principal holds one when its Roles carry the same string.
// New refuses a malformed or repeated one (platform.admin is already registered).
func WithPermissions(permissions ...string) Option {
	return func(o *options) { o.permissions = append(o.permissions, permissions...) }
}

var permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z][a-z0-9_]*$`)

// authzRegistry is what the composition root registered.
type authzRegistry struct {
	classify func(context.Context) string // nil: no kinds registered
	kinds    map[string]PrincipalKind
	// roles maps a permission to the role a principal holds it by.
	roles map[string]string
}

// newAuthzRegistry checks the registrations and builds the registry.
func newAuthzRegistry(adminRole string, o options) (*authzRegistry, error) {
	if adminRole == "" {
		// A config built by hand (tests) skips config.Load's defaults.
		adminRole = config.DefaultAdminRole
	}
	r := &authzRegistry{kinds: map[string]PrincipalKind{}, roles: map[string]string{PlatformAdminPermission: adminRole}}
	if len(o.kinds) > 1 {
		return nil, errors.New("authz: principal kinds are registered twice: pass one WithPrincipalKinds")
	}
	for _, reg := range o.kinds {
		if reg.classify == nil || len(reg.kinds) == 0 {
			return nil, errors.New("authz: WithPrincipalKinds needs both a classifier and at least one kind")
		}
		r.classify = reg.classify
		for _, k := range reg.kinds {
			if k.Name == "" {
				return nil, errors.New("authz: a principal kind has an empty name")
			}
			if _, dup := r.kinds[k.Name]; dup {
				return nil, fmt.Errorf("authz: principal kind %q is registered twice", k.Name)
			}
			r.kinds[k.Name] = k
		}
	}
	for _, p := range o.permissions {
		if !permissionPattern.MatchString(p) {
			return nil, fmt.Errorf("authz: permission %q: want <bc>.<function> matching %s", p, permissionPattern)
		}
		if _, dup := r.roles[p]; dup {
			return nil, fmt.Errorf("authz: permission %q is registered twice", p)
		}
		r.roles[p] = p
	}

	return r, nil
}

// declarationOf reads the declaration a procedure's method descriptor carries.
// It is the one reader: the startup check, auth (is the method public) and the
// authorization interceptor all go through it.
func declarationOf(spec connect.Spec) (*commonv1.MethodAuthz, error) {
	method, ok := spec.Schema.(protoreflect.MethodDescriptor)
	if !ok {
		return nil, errors.New("connect.Spec.Schema holds no protobuf method descriptor")
	}
	opts := method.Options()
	if !proto.HasExtension(opts, commonv1.E_Authz) {
		return nil, errors.New("the method declares no (common.v1.authz) option")
	}
	decl, ok := proto.GetExtension(opts, commonv1.E_Authz).(*commonv1.MethodAuthz)
	if !ok {
		return nil, errors.New("the (common.v1.authz) option is not a common.v1.MethodAuthz")
	}

	return decl, nil
}

// isPublic reports whether the procedure is declared public. A procedure whose
// declaration cannot be read is not.
func isPublic(spec connect.Spec) bool {
	decl, err := declarationOf(spec)

	return err == nil && decl.GetPublic()
}

// check is the startup check of the mounted procedures; the error names every
// procedure that fails it.
func (r *authzRegistry) check(specs []connect.Spec) error {
	var errs []error
	for _, spec := range specs {
		if err := r.checkOne(spec); err != nil {
			errs = append(errs, fmt.Errorf("authz: %s: %w", spec.Procedure, err))
		}
	}

	return errors.Join(errs...)
}

func (r *authzRegistry) checkOne(spec connect.Spec) error {
	decl, err := declarationOf(spec)
	if err != nil {
		return err
	}
	switch policy := decl.GetPolicy().(type) {
	case *commonv1.MethodAuthz_Public:
		if !policy.Public {
			return errors.New("public: false declares nothing: set public: true or rules")
		}

		return nil
	case *commonv1.MethodAuthz_Rules:
		rules := policy.Rules.GetAnyOf()
		if len(rules) == 0 {
			return errors.New("rules holds no rule")
		}
		for i, rule := range rules {
			if err := r.checkRule(rule); err != nil {
				return fmt.Errorf("rule %d: %w", i, err)
			}
		}

		return nil
	default:
		return errors.New("the declaration sets neither public nor rules")
	}
}

func (r *authzRegistry) checkRule(rule *commonv1.AuthzRule) error {
	kind, permission := rule.GetKind(), rule.GetPermission()
	if kind == "" && permission == "" {
		return errors.New("it names neither a kind nor a permission")
	}
	if kind != "" {
		k, ok := r.kinds[kind]
		if !ok {
			return fmt.Errorf("principal kind %q is not registered (registered: %v)", kind, slices.Sorted(maps.Keys(r.kinds)))
		}
		if k.RequiresPermission && permission == "" {
			return fmt.Errorf("principal kind %q must be declared with a permission", kind)
		}
	}
	if permission != "" {
		if _, ok := r.roles[permission]; !ok {
			return fmt.Errorf("permission %q is not registered (registered: %v)", permission, slices.Sorted(maps.Keys(r.roles)))
		}
	}

	return nil
}
