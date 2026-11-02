import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";

export function formatMoney(minor: bigint | undefined, currency: string | undefined): string {
  if (minor === undefined || !currency) return "";
  return `${(Number(minor) / 100).toFixed(2)} ${currency}`;
}

// What the order's payment_required snapshot says (taken when it became CONFIRMED, never
// changed): whether it can only move on through payment.
export function paymentRequiredLabel(required: boolean): string {
  return required ? "Required" : "Not required";
}

// PENDING is waiting on inventory (outline), CONFIRMED and COMPLETED are the good states
// (default), PAID (money received, not yet completed) and CANCELLED are secondary. Shared with
// the order detail page.
export function statusVariant(status: OrderStatus): "default" | "outline" | "secondary" {
  switch (status) {
    case OrderStatus.CONFIRMED:
    case OrderStatus.COMPLETED:
      return "default";
    case OrderStatus.PENDING:
      return "outline";
    default:
      return "secondary";
  }
}
