package main

import (
	"bytes"
	"testing"

	validate "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// This test links the protovalidate Go package pinned in pkgs/platform/go.mod, which is the same
// commit as buf.lock, so "original" below is the schema `buf build` hands to the tool.

// original returns the image `buf build -o -` would produce for a service using buf.validate,
// with validate.proto's dependencies first.
func original(t *testing.T) *descriptorpb.FileDescriptorSet {
	t.Helper()
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imports := fd.Imports()
		for i := range imports.Len() {
			add(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	add(validate.File_buf_validate_validate_proto)

	field := &descriptorpb.FieldOptions{}
	proto.SetExtension(field, validate.E_Field, &validate.FieldRules{
		Type: &validate.FieldRules_String_{String_: &validate.StringRules{MinLen: proto.Uint64(1)}},
	})
	// buf's image extension (field 8042) sits on the file as an unknown field.
	svc := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("shop/v1/service.proto"),
		Package:    proto.String("shop.v1"),
		Dependency: []string{validateFile},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Item"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("sku"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				JsonName: proto.String("sku"),
				Options:  field,
			}},
		}},
		Syntax: proto.String("proto3"),
	}
	svc.ProtoReflect().SetUnknown(protoreflect.RawFields{0xd2, 0xaf, 0x02, 0x00}) // 8042, empty message
	set.File = append(set.File, svc)
	return set
}

func TestStripFiles(t *testing.T) {
	set := original(t)
	if err := Strip(set); err != nil {
		t.Fatal(err)
	}

	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, f := range set.File {
		byName[f.GetName()] = f
	}
	if _, ok := byName[validateFile]; ok {
		t.Errorf("%s is still in the image", validateFile)
	}
	v := byName[violationsFile]
	if v == nil {
		t.Fatalf("%s missing", violationsFile)
	}
	if got, want := v.GetPackage(), "buf.validate"; got != want {
		t.Errorf("package = %q, want %q (type URLs must not change)", got, want)
	}
	var names []string
	for _, m := range v.MessageType {
		names = append(names, m.GetName())
	}
	if want := violationMessages; !equal(names, want) {
		t.Errorf("messages = %v, want %v", names, want)
	}
	if got, want := v.GetOptions().GetJavaPackage(), "build.buf.validate"; got != want {
		t.Errorf("file options not carried over: java_package = %q", got)
	}

	svc := byName["shop/v1/service.proto"]
	if len(svc.Dependency) != 0 {
		t.Errorf("dependency = %v, want none", svc.Dependency)
	}
	if o := svc.MessageType[0].Field[0].GetOptions(); proto.Size(o) != 0 {
		t.Errorf("field options still carry %d bytes", proto.Size(o))
	}
	if len(svc.ProtoReflect().GetUnknown()) == 0 {
		t.Error("buf's file-level extension field was dropped")
	}
}

func TestStripKeepsIsImportOfValidateProto(t *testing.T) {
	set := original(t)
	for _, f := range set.File {
		if f.GetName() == validateFile {
			f.ProtoReflect().SetUnknown(protoreflect.RawFields{0xd2, 0xaf, 0x02, 0x02, 0x08, 0x01})
		}
	}
	if err := Strip(set); err != nil {
		t.Fatal(err)
	}
	for _, f := range set.File {
		if f.GetName() == violationsFile && !bytes.Equal(f.ProtoReflect().GetUnknown(), protoreflect.RawFields{0xd2, 0xaf, 0x02, 0x02, 0x08, 0x01}) {
			t.Errorf("violations.proto lost the image extension of validate.proto: %x", f.ProtoReflect().GetUnknown())
		}
	}
}

func TestStripIsIdempotent(t *testing.T) {
	once := original(t)
	if err := Strip(once); err != nil {
		t.Fatal(err)
	}
	twice := proto.Clone(once).(*descriptorpb.FileDescriptorSet)
	if err := Strip(twice); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(once, twice) {
		t.Error("stripping a stripped image changed it")
	}
}

func TestStripRenumbersDependencyIndexes(t *testing.T) {
	f := &descriptorpb.FileDescriptorProto{
		Dependency:       []string{"a.proto", validateFile, "b.proto", "c.proto"},
		PublicDependency: []int32{0, 1, 3},
		WeakDependency:   []int32{2},
	}
	dropDependency(f, validateFile)
	if want := []string{"a.proto", "b.proto", "c.proto"}; !equal(f.Dependency, want) {
		t.Errorf("dependency = %v, want %v", f.Dependency, want)
	}
	if want := []int32{0, 2}; !equalInt(f.PublicDependency, want) {
		t.Errorf("public = %v, want %v", f.PublicDependency, want)
	}
	if want := []int32{1}; !equalInt(f.WeakDependency, want) {
		t.Errorf("weak = %v, want %v", f.WeakDependency, want)
	}
}

func TestStripFailsOnMissingMessage(t *testing.T) {
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String(validateFile),
		Package: proto.String("buf.validate"),
	}}}
	if err := Strip(set); err == nil {
		t.Error("want an error when validate.proto has no Violations")
	}
}

// violationsJSON exercises every field of the four messages, FieldPathElement's type enums included.
const violationsJSON = `{"violations":[
  {"field":{"elements":[
     {"fieldNumber":2,"fieldName":"lines","fieldType":"TYPE_MESSAGE","index":"0"},
     {"fieldNumber":1,"fieldName":"sku","fieldType":"TYPE_STRING"}]},
   "rule":{"elements":[{"fieldNumber":14,"fieldName":"string","fieldType":"TYPE_MESSAGE"},{"fieldNumber":2,"fieldName":"min_len","fieldType":"TYPE_UINT64"}]},
   "ruleId":"string.min_len","message":"value length must be at least 1 characters","forKey":true},
  {"field":{"elements":[{"fieldNumber":3,"fieldName":"labels","fieldType":"TYPE_MESSAGE","keyType":"TYPE_STRING","valueType":"TYPE_INT32","stringKey":"env"}]},
   "ruleId":"required","message":"value is required"}]}`

// TestWireCompatibility encodes Violations with one schema and decodes with the other, both ways.
func TestWireCompatibility(t *testing.T) {
	origSet := original(t)
	strippedSet := proto.Clone(origSet).(*descriptorpb.FileDescriptorSet)
	if err := Strip(strippedSet); err != nil {
		t.Fatal(err)
	}
	origType := violationsType(t, origSet)
	strippedType := violationsType(t, strippedSet)
	if origType.Descriptor().FullName() != strippedType.Descriptor().FullName() {
		t.Fatalf("full name changed: %s -> %s", origType.Descriptor().FullName(), strippedType.Descriptor().FullName())
	}

	encode := func(typ protoreflect.MessageType) []byte {
		m := typ.New().Interface()
		if err := protojson.Unmarshal([]byte(violationsJSON), m); err != nil {
			t.Fatal(err)
		}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fromOrig, fromStripped := encode(origType), encode(strippedType)
	if !bytes.Equal(fromOrig, fromStripped) {
		t.Errorf("the two schemas encode the same message differently:\n orig     %x\n stripped %x", fromOrig, fromStripped)
	}

	// The generated Go type is a third, independent reader of the original schema.
	generated := &validate.Violations{}
	if err := proto.Unmarshal(fromStripped, generated); err != nil {
		t.Fatalf("generated Go type cannot read stripped encoding: %v", err)
	}
	if got := len(generated.GetViolations()); got != 2 {
		t.Errorf("generated type decoded %d violations, want 2", got)
	}

	for _, tc := range []struct {
		name  string
		bytes []byte
		into  protoreflect.MessageType
	}{
		{"original → stripped", fromOrig, strippedType},
		{"stripped → original", fromStripped, origType},
	} {
		m := tc.into.New().Interface()
		if err := proto.Unmarshal(tc.bytes, m); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(m.ProtoReflect().GetUnknown()) != 0 {
			t.Errorf("%s: decoded with unknown fields %x", tc.name, m.ProtoReflect().GetUnknown())
		}
		back, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, tc.bytes) {
			t.Errorf("%s: re-encoding differs:\n got  %x\n want %x", tc.name, back, tc.bytes)
		}
	}
}

func violationsType(t *testing.T, set *descriptorpb.FileDescriptorSet) protoreflect.MessageType {
	t.Helper()
	files, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("buf.validate.Violations")
	if err != nil {
		t.Fatal(err)
	}
	return dynamicpb.NewMessageType(d.(protoreflect.MessageDescriptor))
}

func equal(a, b []string) bool {
	return len(a) == len(b) && func() bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}()
}

func equalInt(a, b []int32) bool {
	return len(a) == len(b) && func() bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}()
}
