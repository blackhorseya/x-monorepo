package servertest

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"

	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

// Public is the declaration of a method anyone may call.
func Public() *commonv1.MethodAuthz {
	return &commonv1.MethodAuthz{Policy: &commonv1.MethodAuthz_Public{Public: true}}
}

// Rules is the declaration of a method a caller meeting any one of rules may call.
func Rules(rules ...*commonv1.AuthzRule) *commonv1.MethodAuthz {
	return &commonv1.MethodAuthz{Policy: &commonv1.MethodAuthz_Rules{Rules: &commonv1.AuthzRules{AnyOf: rules}}}
}

// Spec is a fixture procedure for a test that mounts its own method: a unary "/<package>.<Service>/<Method>" whose
// Schema is a real method descriptor, as generated code's is, carrying decl as its (common.v1.authz) option (nil =
// no option). The server reads it the way it reads a generated one. It panics on a procedure of another shape.
func Spec(procedure string, decl *commonv1.MethodAuthz) connect.Spec {
	return connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: procedure, Schema: methodDescriptor(procedure, decl)}
}

func methodDescriptor(procedure string, decl *commonv1.MethodAuthz) protoreflect.MethodDescriptor {
	full, method, ok := strings.Cut(strings.TrimPrefix(procedure, "/"), "/")
	dot := strings.LastIndexByte(full, '.')
	if !ok || dot < 0 {
		panic(fmt.Sprintf("servertest: procedure %q is not /<package>.<Service>/<Method>", procedure))
	}
	var opts *descriptorpb.MethodOptions
	if decl != nil {
		opts = &descriptorpb.MethodOptions{}
		proto.SetExtension(opts, commonv1.E_Authz, decl)
	}
	empty := string((&emptypb.Empty{}).ProtoReflect().Descriptor().FullName())
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("servertest/" + full + "/" + method + ".proto"),
		Package:    proto.String(full[:dot]),
		Syntax:     proto.String("proto3"),
		Dependency: []string{(&emptypb.Empty{}).ProtoReflect().Descriptor().ParentFile().Path()},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String(full[dot+1:]),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name: proto.String(method), InputType: proto.String("." + empty), OutputType: proto.String("." + empty), Options: opts,
			}},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		panic(fmt.Sprintf("servertest: descriptor of %q: %v", procedure, err))
	}

	return file.Services().Get(0).Methods().Get(0)
}
