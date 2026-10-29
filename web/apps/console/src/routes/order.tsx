import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus, type Order } from "@repo/proto/ordering/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useResetOnCredentialChange } from "../auth";
import { ConfirmButton } from "../components/confirm";
import { describe, orders } from "../client";
import { keys, orderQuery, reservationQuery } from "../queries";
import { ErrorAlert, formatMoney, statusVariant } from "./orders";
import { reservationBadge } from "./reservations";

// /orders/$id: one order in full — every line, the total, and what inventory
// decided for it. The list shows 8 characters of the id and the first badge;
// this is where the rest lives, and where a shared link lands.
export function OrderPage() {
  const { id } = useParams({ from: "/orders/$id" });
  const { data, isError, error } = useQuery(orderQuery(id));

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle className="font-mono text-sm">{id}</CardTitle>
          <CardDescription>
            <Link to="/orders" search={{}} className="underline">
              All orders
            </Link>
            {data &&
              ` · customer ${data.customerId} · created ${at(
                data.createTime
              )} · updated ${at(data.updateTime)}`}
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {isError && <ErrorAlert error={error} />}
          {data === null && (
            <p className="text-sm text-muted-foreground">
              No such order — it was never created, or the server restarted with
              in-memory storage.
            </p>
          )}
          {data && <OrderDetail order={data} />}
        </CardContent>
      </Card>
      {data && <ReservationCard orderId={id} />}
    </div>
  );
}

function OrderDetail({ order }: { order: Order }) {
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: () => orders.cancelOrder({ id: order.id }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.order(order.id) });
      void qc.invalidateQueries({ queryKey: keys.reservation(order.id) });
    },
  });
  const complete = useMutation({
    mutationFn: () => orders.completeOrder({ id: order.id }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.order(order.id) });
      void qc.invalidateQueries({ queryKey: keys.reservation(order.id) });
    },
  });
  useResetOnCredentialChange(cancel);
  useResetOnCredentialChange(complete);
  const open = order.status === OrderStatus.PENDING || order.status === OrderStatus.CONFIRMED;
  const confirmed = order.status === OrderStatus.CONFIRMED;
  const busy = cancel.isPending || complete.isPending;

  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <Badge
          variant={statusVariant(order.status)}
          data-testid="order-status"
        >
          {OrderStatus[order.status]}
        </Badge>
        {confirmed && (
          <ConfirmButton
            label="Complete"
            variant="default"
            title="Complete this order?"
            description="This cannot be undone: inventory commits the reservation, so the stock is sold, not just held."
            action="Complete order"
            onConfirm={() => complete.mutateAsync()}
            disabled={busy}
          />
        )}
        {open && (
          <ConfirmButton
            label="Cancel"
            title="Cancel this order?"
            description="The order is cancelled and inventory releases what it reserved."
            action="Cancel order"
            onConfirm={() => cancel.mutateAsync()}
            disabled={busy}
          />
        )}
        {cancel.isError && (
          <p className="text-xs text-destructive">{describe(cancel.error)}</p>
        )}
        {complete.isError && (
          <p className="text-xs text-destructive">{describe(complete.error)}</p>
        )}
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>SKU</TableHead>
            <TableHead className="text-right">Qty</TableHead>
            <TableHead className="text-right">Unit price</TableHead>
            <TableHead className="text-right">Line total</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {order.items.map((it, i) => (
            <TableRow key={i}>
              <TableCell className="font-mono text-xs">{it.sku}</TableCell>
              <TableCell className="text-right tabular-nums">
                {it.quantity}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {formatMoney(it.unitPrice?.amountMinor, it.unitPrice?.currency)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {formatMoney(
                  it.unitPrice
                    ? it.unitPrice.amountMinor * BigInt(it.quantity)
                    : undefined,
                  it.unitPrice?.currency
                )}
              </TableCell>
            </TableRow>
          ))}
          <TableRow data-testid="order-total">
            <TableCell colSpan={3} className="text-right font-medium">
              Total
            </TableCell>
            <TableCell className="text-right font-medium tabular-nums">
              {formatMoney(order.total?.amountMinor, order.total?.currency)}
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </>
  );
}

// What inventory decided: the same query the list's Reservation column polls,
// here with the lines and the shortfall spelled out.
function ReservationCard({ orderId }: { orderId: string }) {
  const { data, isError, error } = useQuery(reservationQuery(orderId));
  return (
    <Card>
      <CardHeader>
        <CardTitle>Reservation</CardTitle>
        <CardDescription>
          Made by <code>apps/worker</code> when it consumes OrderCreated;
          released when the order is cancelled.{" "}
          <Link
            to="/reservations"
            search={{ order: orderId }}
            className="underline"
          >
            Open in Reservations
          </Link>
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isError && <ErrorAlert error={error} />}
        {data === null && (
          <p className="text-sm text-muted-foreground">none yet</p>
        )}
        {data && (
          <div className="grid gap-2 text-sm">
            <span className="flex flex-wrap items-center gap-2">
              <Badge variant={reservationBadge[data.status]}>
                {ReservationStatus[data.status]}
              </Badge>
              {data.shortfallSkus.length > 0 && (
                <span className="text-xs text-muted-foreground">
                  short: {data.shortfallSkus.join(", ")}
                </span>
              )}
            </span>
            <p className="text-xs text-muted-foreground">
              {data.lines.length === 0
                ? "no lines"
                : data.lines
                    .map((l) => `${l.sku} × ${l.quantity}`)
                    .join(", ")}{" "}
              · updated {at(data.updateTime)}
            </p>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

const at = (t?: { seconds: bigint }) =>
  t ? new Date(Number(t.seconds) * 1000).toLocaleString() : "";
