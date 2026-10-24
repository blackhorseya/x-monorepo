import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Plus, Trash2 } from "lucide-react";
import { cloneElement, useId, useState, type ReactElement } from "react";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus, type Order } from "@repo/proto/ordering/v1/entity_pb";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useResetOnCredentialChange } from "../auth";
import { describe, isUnauthenticated, orders } from "../client";
import { keys, ordersQuery, reservationQuery } from "../queries";
import { reservationBadge } from "./reservations";

type Line = { sku: string; quantity: number; priceMinor: number };

// Search params of /orders: a status filter and a customer id filter.
export type OrdersSearch = { status?: "PENDING" | "CANCELLED"; customer?: string };

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
  const [customerId, setCustomerId] = useState("c-1");
  const [currency, setCurrency] = useState("USD");
  const [lines, setLines] = useState<Line[]>([{ sku: "SKU-1", quantity: 2, priceMinor: 1299 }]);

  const create = useMutation({
    mutationFn: () =>
      orders.createOrder({
        customerId,
        items: lines.map((l) => ({
          sku: l.sku,
          quantity: l.quantity,
          unitPrice: { amountMinor: BigInt(l.priceMinor), currency },
        })),
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
          Inventory reserves stock asynchronously; reservations need <code>apps/worker</code> and Postgres.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex flex-wrap gap-4">
          <Field label="Customer" className="w-48">
            <Input value={customerId} onChange={(e) => setCustomerId(e.target.value)} />
          </Field>
          <Field label="Currency" className="w-24">
            <Input value={currency} onChange={(e) => setCurrency(e.target.value)} />
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
            <Field label="Unit price (minor)" className="w-36">
              <Input
                type="number"
                value={l.priceMinor}
                onChange={(e) => setLine(i, { priceMinor: Number(e.target.value) })}
              />
            </Field>
            <Button variant="ghost" size="icon" aria-label="remove line" onClick={() => setLines(lines.filter((_, j) => j !== i))}>
              <Trash2 />
            </Button>
          </div>
        ))}
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setLines([...lines, { sku: "", quantity: 1, priceMinor: 100 }])}>
            <Plus /> Line
          </Button>
          <Button onClick={() => create.mutate()} disabled={create.isPending}>
            Create order
          </Button>
        </div>
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
  const list = useInfiniteQuery(ordersQuery(customer, status));
  const rows = list.data?.pages.flatMap((p) => p.orders) ?? [];
  const setSearch = (patch: Partial<OrdersSearch>) => void navigate({ search: (prev) => ({ ...prev, ...patch }) });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Orders</CardTitle>
        <CardDescription>Newest first, 20 per page, refreshed every 2s.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex flex-wrap items-end gap-4">
          <div className="flex gap-2">
            {(["ANY", "PENDING", "CANCELLED"] as const).map((s) => {
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
              <OrderRow key={o.id} order={o} />
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

function OrderRow({ order }: { order: Order }) {
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: () => orders.cancelOrder({ id: order.id }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.orders });
      void qc.invalidateQueries({ queryKey: keys.reservation(order.id) });
    },
  });
  useResetOnCredentialChange(cancel);
  const pending = order.status === OrderStatus.PENDING;

  return (
    <TableRow>
      <TableCell className="font-mono text-xs">{order.id.slice(0, 8)}</TableCell>
      <TableCell>{order.customerId}</TableCell>
      <TableCell className="text-right tabular-nums">{formatMoney(order.total?.amountMinor, order.total?.currency)}</TableCell>
      <TableCell>
        <Badge variant={pending ? "default" : "secondary"}>{OrderStatus[order.status]}</Badge>
      </TableCell>
      <TableCell>
        <ReservationCell orderId={order.id} />
      </TableCell>
      <TableCell className="text-right">
        {pending && (
          <Button variant="outline" size="sm" onClick={() => cancel.mutate()} disabled={cancel.isPending}>
            Cancel
          </Button>
        )}
        {cancel.isError && <p className="text-xs text-destructive">{describe(cancel.error)}</p>}
      </TableCell>
    </TableRow>
  );
}

function ReservationCell({ orderId }: { orderId: string }) {
  const { data, isError, error } = useQuery(reservationQuery(orderId));
  if (isError) return <span className="text-xs text-destructive">{describe(error)}</span>;
  if (data === undefined) return null;
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

// Field labels one input: the id ties <label for> to it, so clicking the label
// focuses the input and screen readers (and getByLabel) can find it.
export function Field({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  children: ReactElement<{ id?: string }>;
}) {
  const id = useId();
  return (
    <div className={cn("grid gap-1.5", className)}>
      <Label htmlFor={id}>{label}</Label>
      {cloneElement(children, { id })}
    </div>
  );
}

export function ErrorAlert({ error }: { error: unknown }) {
  const unauthenticated = isUnauthenticated(error);
  return (
    <Alert variant="destructive">
      <AlertTitle>{unauthenticated ? "Not authenticated" : "Request failed"}</AlertTitle>
      <AlertDescription>
        {describe(error)}
        {unauthenticated && " — the server runs with auth on; set an API key or JWT with the Credential button above."}
      </AlertDescription>
    </Alert>
  );
}

function formatMoney(minor: bigint | undefined, currency: string | undefined): string {
  if (minor === undefined || !currency) return "";
  return `${(Number(minor) / 100).toFixed(2)} ${currency}`;
}
