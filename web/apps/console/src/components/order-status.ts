import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";

export function formatMoney(minor: bigint | undefined, currency: string | undefined): string {
  if (minor === undefined || !currency) return "";
  return `${(Number(minor) / 100).toFixed(2)} ${currency}`;
}

// PENDING is waiting on inventory (outline), CONFIRMED and COMPLETED are the good states
// (default), CANCELLED is over (secondary). Shared with the order detail page.
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
