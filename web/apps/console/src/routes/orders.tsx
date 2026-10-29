import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Plus, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus, type Order } from "@repo/proto/ordering/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useResetOnCredentialChange } from "../auth";
import { ConfirmButton } from "../components/confirm";
import { useCanChooseCustomer } from "../components/customer";
import { ErrorAlert } from "../components/error-alert";
import { Field } from "../components/field";
import { formatMoney, statusVariant } from "../components/order-status";
import { describe, orders } from "../client";
import { useOrderReservations, type RowReservation } from "../orderReservations";
import { keys, ordersQuery, quoteQuery, refreshFirstPage } from "../queries";
import { reservationBadge } from "./reservations";

type Line = { sku: string; quantity: number };

// useDebounced follows value after it has stopped changing for ms: the quote is asked once the
// user stops typing, not once per keystroke.
function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

// What a line shows: the catalog's price, or why there is none. "pending" covers both the first
// read and a SKU the last answer did not cover yet.
type Quoted = { kind: "price"; amountMinor: bigint; currency: string } | { kind: "missing" } | { kind: "inactive" } | { kind: "pending" } | { kind: "unavailable" };

function quoteLabel(q: Quoted): string {
  switch (q.kind) {
    case "price":
      return `${formatMoney(q.amountMinor, q.currency)} each`;
    case "missing":
      return "not in the catalog";
    case "inactive":
      return "not on sale";
    case "unavailable":
      return "price unavailable";
    default:
      return "…";
  }
}

// totalLabel is the preview of the order's total: only when every named line has a price in one
// currency (an order takes one, the server refuses a mix).
function totalLabel(lines: Line[], quoted: Quoted[]): string {
  const named = quoted.map((q, i) => ({ q, qty: lines[i].quantity })).filter((_, i) => lines[i].sku.trim() !== "");
  if (named.length === 0) return "";
  if (named.some(({ q }) => q.kind !== "price")) return "Total once every line has a price";
  const priced = named as { q: Extract<Quoted, { kind: "price" }>; qty: number }[];
  const currency = priced[0].q.currency;
  if (priced.some(({ q }) => q.currency !== currency)) return "An order takes one currency: these lines are priced in more than one";
  if (priced.some(({ qty }) => !Number.isInteger(qty) || qty <= 0)) return "Total once every quantity is a positive whole number";
  const total = priced.reduce((sum, { q, qty }) => sum + q.amountMinor * BigInt(qty), 0n);
  return `Estimated total ${formatMoney(total, currency)}`;
}

// Search params of /orders: a status filter and a customer id filter.
export type OrdersSearch = { status?: "PENDING" | "CONFIRMED" | "COMPLETED" | "CANCELLED"; customer?: string };

export function OrdersPage() {
  return (
    <div className="grid gap-6">
      <CreateOrderCard />
      <OrdersCard />
    </div>
  );
}

function CreateOrderCard() {
  const qc = useQueryClient();
  // Optional: empty = the caller. Only an admin (on behalf of a customer) or an anonymous caller
  // (who has nobody to default to) may name one; for anyone else the server uses their own
  // subject and refuses another, so the field is locked and nothing is sent.
  const canChoose = useCanChooseCustomer();
  const [customerId, setCustomerId] = useState("");
  const [lines, setLines] = useState<Line[]>([{ sku: "SKU-1", quantity: 2 }]);

  // Read-only preview of what each line costs now. It never goes back to the server as a price:
  // CreateOrder sends sku and quantity only, and the order keeps the price found when it is placed.
  const asked = useDebounced(
    [...new Set(lines.map((l) => l.sku.trim()).filter((s) => s !== ""))].sort(),
    250,
  );
  const quote = useQuery(quoteQuery(asked));
  const quoted: Quoted[] = lines.map((l) => {
    const sku = l.sku.trim();
    const data = quote.data;
    if (sku === "") return { kind: "pending" };
    if (data?.prices[sku]) return { kind: "price", amountMinor: data.prices[sku].amountMinor, currency: data.prices[sku].currency };
    if (data?.missing.includes(sku)) return { kind: "missing" };
    if (data?.inactive.includes(sku)) return { kind: "inactive" };
    return quote.isError ? { kind: "unavailable" } : { kind: "pending" };
  });
  const total = totalLabel(lines, quoted);

  const create = useMutation({
    mutationFn: () =>
      orders.createOrder({
        customerId: canChoose ? customerId.trim() : "",
        items: lines.map((l) => ({ sku: l.sku, quantity: l.quantity })),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.orders }),
  });
  useResetOnCredentialChange(create);

  const setLine = (i: number, patch: Partial<Line>) =>
    setLines(lines.map((l, j) => (j === i ? { ...l, ...patch } : l)));

  return (
    <Card>
      <CardHeader>
        <CardTitle>Create order</CardTitle>
        <CardDescription>
          Prices come from the catalog (Products), not from you: the order keeps the price each SKU had when it was placed.
          Inventory reserves stock asynchronously; reservations need <code>apps/worker</code> and Postgres.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex flex-wrap gap-4">
          <Field label="Customer" className="w-48">
            <Input
              value={canChoose ? customerId : ""}
              placeholder={canChoose ? "optional: you, if empty" : "your account"}
              disabled={!canChoose}
              onChange={(e) => setCustomerId(e.target.value)}
            />
          </Field>
        </div>
        {lines.map((l, i) => (
          <div key={i} className="flex flex-wrap items-end gap-2">
            <Field label="SKU" className="w-40">
              <Input value={l.sku} onChange={(e) => setLine(i, { sku: e.target.value })} />
            </Field>
            <Field label="Qty" className="w-20">
              <Input type="number" value={l.quantity} onChange={(e) => setLine(i, { quantity: Number(e.target.value) })} />
            </Field>
            {l.sku.trim() !== "" && (
              <span data-testid="line-price" className="pb-2 text-sm tabular-nums text-muted-foreground">
                {quoteLabel(quoted[i])}
              </span>
            )}
            <Button variant="ghost" size="icon" aria-label="remove line" onClick={() => setLines(lines.filter((_, j) => j !== i))}>
              <Trash2 />
            </Button>
          </div>
        ))}
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setLines([...lines, { sku: "", quantity: 1 }])}>
            <Plus /> Line
          </Button>
          <Button onClick={() => create.mutate()} disabled={create.isPending}>
            Create order
          </Button>
        </div>
        {total !== "" && (
          <p data-testid="quote-total" className="text-sm text-muted-foreground">
            {total}
          </p>
        )}
        {create.isError && <ErrorAlert error={create.error} />}
      </CardContent>
    </Card>
  );
}

function OrdersCard() {
  const search = useSearch({ from: "/orders" });
  const navigate = useNavigate({ from: "/orders" });
  const status = search.status ? OrderStatus[search.status] : OrderStatus.UNSPECIFIED;
  const customer = search.customer ?? "";
  const [customerInput, setCustomerInput] = useState(customer);
  const [orderIdInput, setOrderIdInput] = useState("");
  const list = useInfiniteQuery(ordersQuery(customer, status));
  const qc = useQueryClient();
  // Only the newest page polls (see refreshFirstPage); a hidden tab does not.
  useEffect(() => {
    const t = setInterval(() => {
      if (document.visibilityState === "visible") void refreshFirstPage(qc, customer, status).catch(() => {});
    }, 2000);
    return () => clearInterval(t);
  }, [qc, customer, status]);
  const rows = list.data?.pages.flatMap((p) => p.orders) ?? [];
  const reservations = useOrderReservations(rows);
  const setSearch = (patch: Partial<OrdersSearch>) => void navigate({ search: (prev) => ({ ...prev, ...patch }) });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Orders</CardTitle>
        <CardDescription>Newest first, 20 per page; the newest page refreshes every 2s.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex flex-wrap items-end gap-4">
          <div className="flex gap-2">
            {(["ANY", "PENDING", "CONFIRMED", "COMPLETED", "CANCELLED"] as const).map((s) => {
              const active = s === "ANY" ? !search.status : search.status === s;
              return (
                <Button key={s} size="sm" variant={active ? "default" : "outline"} onClick={() => setSearch({ status: s === "ANY" ? undefined : s })}>
                  {s}
                </Button>
              );
            })}
          </div>
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              setSearch({ customer: customerInput.trim() || undefined });
            }}
          >
            <Field label="Customer" className="w-48">
              <Input value={customerInput} placeholder="any" onChange={(e) => setCustomerInput(e.target.value)} />
            </Field>
            <Button type="submit" variant="outline" size="sm">
              Filter
            </Button>
            {customer && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => {
                  setCustomerInput("");
                  setSearch({ customer: undefined });
                }}
              >
                Clear
              </Button>
            )}
          </form>
          {/* A full order id (from Reservations, a log line, a support ticket) goes straight to
              /orders/$id; the list only shows the first 8 characters and may not have the page. */}
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const id = orderIdInput.trim();
              if (id) void navigate({ to: "/orders/$id", params: { id } });
            }}
          >
            <Field label="Order id" className="w-72">
              <Input value={orderIdInput} placeholder="paste a full id" onChange={(e) => setOrderIdInput(e.target.value)} />
            </Field>
            <Button type="submit" variant="outline" size="sm" disabled={!orderIdInput.trim()}>
              Open
            </Button>
          </form>
        </div>
        {list.isError && <ErrorAlert error={list.error} />}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Customer</TableHead>
              <TableHead className="text-right">Total</TableHead>
              <TableHead>Order</TableHead>
              <TableHead>Reservation</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((o) => (
              <OrderRow key={o.id} order={o} reservation={reservations.known.get(o.id)} reservationError={reservations.error} />
            ))}
            {list.isSuccess && rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-muted-foreground">
                  No orders{search.status ? ` with status ${search.status}` : ""}{customer ? ` for ${customer}` : ""} yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        {list.hasNextPage && (
          <Button variant="outline" size="sm" className="justify-self-start" onClick={() => void list.fetchNextPage()} disabled={list.isFetchingNextPage}>
            Load more
          </Button>
        )}
      </CardContent>
    </Card>
  );
}

function OrderRow({ order, reservation, reservationError }: { order: Order; reservation?: RowReservation; reservationError: unknown }) {
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: () => orders.cancelOrder({ id: order.id }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.order(order.id) });
    },
  });
  const complete = useMutation({
    mutationFn: () => orders.completeOrder({ id: order.id }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.order(order.id) });
    },
  });
  useResetOnCredentialChange(cancel);
  useResetOnCredentialChange(complete);
  // PENDING and CONFIRMED (inventory reserved every line) are both still the customer's to
  // cancel; inventory releases the reservation on OrderCancelled. Only CONFIRMED can be
  // completed: inventory commits the reservation on OrderCompleted (sold, not held).
  const open = order.status === OrderStatus.PENDING || order.status === OrderStatus.CONFIRMED;
  const confirmed = order.status === OrderStatus.CONFIRMED;
  const busy = cancel.isPending || complete.isPending;

  return (
    <TableRow>
      <TableCell className="font-mono text-xs">
        <Link to="/orders/$id" params={{ id: order.id }} title={order.id} className="underline">
          {order.id.slice(0, 8)}
        </Link>
      </TableCell>
      <TableCell>{order.customerId}</TableCell>
      <TableCell className="text-right tabular-nums">{formatMoney(order.total?.amountMinor, order.total?.currency)}</TableCell>
      <TableCell>
        <Badge variant={statusVariant(order.status)}>{OrderStatus[order.status]}</Badge>
      </TableCell>
      <TableCell>
        <ReservationCell orderId={order.id} state={reservation} error={reservationError} />
      </TableCell>
      <TableCell className="space-x-2 text-right">
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
        {cancel.isError && <p className="text-xs text-destructive">{describe(cancel.error)}</p>}
        {complete.isError && <p className="text-xs text-destructive">{describe(complete.error)}</p>}
      </TableCell>
    </TableRow>
  );
}

function ReservationCell({ orderId, state, error }: { orderId: string; state?: RowReservation; error: unknown }) {
  // The list asks for every waiting row's reservation at once (useOrderReservations); this only shows it.
  if (state === undefined) return error === undefined ? null : <span className="text-xs text-destructive">{describe(error)}</span>;
  const data = state.reservation;
  if (data === null) return <span className="text-sm text-muted-foreground">none yet</span>;
  return (
    <span className="flex flex-wrap items-center gap-2">
      <Link to="/reservations" search={{ order: orderId }} title="Open in Reservations">
        <Badge variant={reservationBadge[data.status]}>{ReservationStatus[data.status]}</Badge>
      </Link>
      {data.shortfallSkus.length > 0 && (
        <span className="text-xs text-muted-foreground">short: {data.shortfallSkus.join(", ")}</span>
      )}
    </span>
  );
}
