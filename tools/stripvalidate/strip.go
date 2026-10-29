package main

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	validateFile   = "buf/validate/validate.proto"
	violationsFile = "buf/validate/violations.proto"
)

// violationMessages are the only messages of validate.proto the console decodes at runtime
// (ViolationsSchema in web/apps/console/src/client.ts), in their original order. The package stays
// buf.validate, so type URLs (type.googleapis.com/buf.validate.Violations) do not change.
var violationMessages = []string{"Violations", "Violation", "FieldPath", "FieldPathElement"}

// Strip rewrites set so the generated TypeScript no longer carries buf.validate's ~70 kB
// descriptor: every options message loses its extension and unknown fields (that is where
// (buf.validate.field) and friends live), nothing depends on validate.proto any more, and
// validate.proto itself becomes violations.proto with just violationMessages. Everything
// else, buf's own extension fields on FileDescriptorProto included, passes through untouched.
func Strip(set *descriptorpb.FileDescriptorSet) error {
	for i, f := range set.GetFile() {
		scrubOptions(f.ProtoReflect())
		if f.GetName() == validateFile {
			v, err := violations(f)
			if err != nil {
				return err
			}
			set.File[i] = v
			continue
		}
		dropDependency(f, validateFile)
	}

	// The result must still be a closed, linkable set: anything left that referred to a
	// stripped message (or to validate.proto) fails here instead of in the TS plugin.
	if _, err := protodesc.NewFiles(set); err != nil {
		return fmt.Errorf("stripped image does not link: %w", err)
	}
	return nil
}

// violations builds violations.proto out of validate.proto, keeping its syntax/edition and file
// options and the unknown fields (buf's is_import flag), so it is emitted exactly when validate.proto was.
func violations(orig *descriptorpb.FileDescriptorProto) (*descriptorpb.FileDescriptorProto, error) {
	v := &descriptorpb.FileDescriptorProto{
		Name:       proto.String(violationsFile),
		Package:    orig.Package,
		Dependency: []string{"google/protobuf/descriptor.proto"}, // FieldPathElement.field_type
		Syntax:     orig.Syntax,
		Edition:    orig.Edition,
		Options:    proto.Clone(orig.GetOptions()).(*descriptorpb.FileOptions),
	}
	v.ProtoReflect().SetUnknown(orig.ProtoReflect().GetUnknown())

	for _, name := range violationMessages {
		i := slices.IndexFunc(orig.GetMessageType(), func(m *descriptorpb.DescriptorProto) bool { return m.GetName() == name })
		if i < 0 {
			return nil, fmt.Errorf("%s has no message %s", validateFile, name)
		}
		v.MessageType = append(v.MessageType, proto.Clone(orig.MessageType[i]).(*descriptorpb.DescriptorProto))
	}
	scrubOptions(v.ProtoReflect())
	return v, nil
}

// dropDependency removes dep from f and renumbers public_dependency / weak_dependency,
// which are indexes into the dependency list.
func dropDependency(f *descriptorpb.FileDescriptorProto, dep string) {
	at := slices.Index(f.Dependency, dep)
	if at < 0 {
		return
	}
	f.Dependency = slices.Delete(f.Dependency, at, at+1)
	f.PublicDependency = reindex(f.PublicDependency, int32(at))
	f.WeakDependency = reindex(f.WeakDependency, int32(at))
}

func reindex(idx []int32, removed int32) []int32 {
	var out []int32
	for _, i := range idx {
		switch {
		case i < removed:
			out = append(out, i)
		case i > removed:
			out = append(out, i-1)
		}
	}
	return out
}

// scrubOptions walks m and clears extensions and unknown fields on every google.protobuf.*Options
// message below it. Registered extensions (a binary that links protovalidate) and unknown ones
// (this tool as shipped) are both removed, so the result does not depend on what the binary links.
func scrubOptions(m protoreflect.Message) {
	if isOptions(m.Descriptor()) {
		m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			if fd.IsExtension() {
				m.Clear(fd)
			}
			return true
		})
		m.SetUnknown(nil)
	}

	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil || fd.IsMap() {
			return true
		}
		if fd.IsList() {
			for i := range v.List().Len() {
				scrubOptions(v.List().Get(i).Message())
			}
			return true
		}
		scrubOptions(v.Message())
		return true
	})
}

func isOptions(d protoreflect.MessageDescriptor) bool {
	name := string(d.FullName())
	return strings.HasPrefix(name, "google.protobuf.") && strings.HasSuffix(name, "Options")
}
