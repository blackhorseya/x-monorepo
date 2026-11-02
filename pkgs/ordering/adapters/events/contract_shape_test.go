package events_test

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
)

// The wire contract of docs/payment.md §2 / §4, read through protobuf reflection
// so this file compiles against any version of the generated code.
func TestContract_PaymentShape(t *testing.T) {
	t.Run("OrderStatus has PAID", func(t *testing.T) {
		if _, ok := orderingv1.OrderStatus_value["ORDER_STATUS_PAID"]; !ok {
			t.Fatal("ordering.v1.OrderStatus has no ORDER_STATUS_PAID")
		}
	})

	fieldOf := func(t *testing.T, msg protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
		t.Helper()
		f := msg.Fields().ByName(protoreflect.Name(name))
		if f == nil {
			t.Fatalf("%s has no field %q", msg.FullName(), name)
		}
		return f
	}

	t.Run("Order carries payment_required", func(t *testing.T) {
		f := fieldOf(t, (&orderingv1.Order{}).ProtoReflect().Descriptor(), "payment_required")
		if f.Kind() != protoreflect.BoolKind {
			t.Fatalf("Order.payment_required kind = %v, want bool", f.Kind())
		}
	})

	t.Run("OrderConfirmed carries total as common.v1.Money", func(t *testing.T) {
		f := fieldOf(t, (&orderingv1.OrderConfirmed{}).ProtoReflect().Descriptor(), "total")
		if f.Message() == nil || f.Message().FullName() != "common.v1.Money" {
			t.Fatalf("OrderConfirmed.total type = %v, want common.v1.Money", f.Message())
		}
	})

	t.Run("OrderPaid event exists", func(t *testing.T) {
		if _, err := protoregistry.GlobalTypes.FindMessageByName("ordering.v1.OrderPaid"); err != nil {
			t.Fatalf("ordering.v1.OrderPaid is not registered: %v", err)
		}
	})
}
