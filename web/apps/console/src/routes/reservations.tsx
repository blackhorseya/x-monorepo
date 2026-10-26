import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { ReservationStatus, type Reservation } from "@repo/proto/inventory/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { reservationQuery, reservationsQuery } from "../queries";
import { ErrorAlert } from "./orders";

// Search params of /reservations: a status filter and, from the Orders page,
// the order whose reservation to pin at the top.
export type ReservationsSearch = { status?: keyof typeof ReservationStatus; order?: string };

const statuses = ["RESERVED", "REJECTED", "RELEASED", "COMMITTED", "EXPIRED", "CANCELLED"] as const;

export const reservationBadge: Record<ReservationStatus, "default" | "secondary" | "destructive" | "outline"> = {
  [ReservationStatus.UNSPECIFIED]: "outline",
  [ReservationStatus.RESERVED]: "default",
  [ReservationStatus.REJECTED]: "destructive",
  [ReservationStatus.RELEASED]: "secondary",
  [ReservationStatus.COMMITTED]: "default",
  [ReservationStatus.EXPIRED]: "destructive",
  [ReservationStatus.CANCELLED]: "outline",
};

export function ReservationsPage() {
  const search = useSearch({ from: "/reservations" });
  const navigate = useNavigate({ from: "/reservations" });
  const status = search.status ? ReservationStatus[search.status] : ReservationStatus.UNSPECIFIED;
  const list = useInfiniteQuery(reservationsQuery(status));
  const rows = list.data?.pages.flatMap((p) => p.reservations) ?? [];

  return (
    <div className="grid gap-6">
      {search.order && <PinnedReservation orderId={search.order} />}
      <Card>
        <CardHeader>
          <CardTitle>Reservations</CardTitle>
          <CardDescription>
            What inventory decided for each order, newest first, refreshed every 2s. Reservations are made by{" "}
            <code>apps/worker</code> when it consumes OrderCreated.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="flex flex-wrap gap-2">
            {(["ANY", ...statuses] as const).map((s) => {
              const active = s === "ANY" ? !search.status : search.status === s;
              return (
                <Button
                  key={s}
                  size="sm"
                  variant={active ? "default" : "outline"}
                  onClick={() => void navigate({ search: (prev) => ({ ...prev, status: s === "ANY" ? undefined : s }) })}
                >
                  {s}
                </Button>
              );
            })}
          </div>
          {list.isError && <ErrorAlert error={list.error} />}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Order</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Lines</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Updated</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <ReservationRow key={r.orderId} reservation={r} />
              ))}
              {list.isSuccess && rows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="text-center text-muted-foreground">
                    No reservations{search.status ? ` with status ${search.status}` : ""} yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
          {list.hasNextPage && (
            <Button
              variant="outline"
              size="sm"
              className="justify-self-start"
              onClick={() => void list.fetchNextPage()}
              disabled={list.isFetchingNextPage}
            >
              Load more
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// PinnedReservation shows the one order the user came from (Orders → badge),
// whatever page of the list it is on.
function PinnedReservation({ orderId }: { orderId: string }) {
  const { data, isError, error } = useQuery(reservationQuery(orderId));
  return (
    <Card>
      <CardHeader>
        <CardTitle className="font-mono text-sm">{orderId}</CardTitle>
        <CardDescription>
          From the Orders page.{" "}
          <Link to="/reservations" search={{}} className="underline">
            Clear
          </Link>
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isError && <ErrorAlert error={error} />}
        {data === null && <p className="text-sm text-muted-foreground">No reservation yet — the worker has not consumed this order.</p>}
        {data && (
          <Table>
            <TableBody>
              <ReservationRow reservation={data} />
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function ReservationRow({ reservation: r }: { reservation: Reservation }) {
  const at = (t?: { seconds: bigint }) => (t ? new Date(Number(t.seconds) * 1000).toLocaleString() : "");
  return (
    <TableRow>
      <TableCell className="font-mono text-xs">
        <Link to="/orders/$id" params={{ id: r.orderId }} className="underline">
          {r.orderId}
        </Link>
      </TableCell>
      <TableCell>
        <span className="flex flex-wrap items-center gap-2">
          <Badge variant={reservationBadge[r.status]}>{ReservationStatus[r.status]}</Badge>
          {r.shortfallSkus.length > 0 && <span className="text-xs text-muted-foreground">short: {r.shortfallSkus.join(", ")}</span>}
        </span>
      </TableCell>
      <TableCell className="text-xs">
        {r.lines.length === 0 ? (
          <span className="text-muted-foreground">none</span>
        ) : (
          r.lines.map((l) => `${l.sku} × ${l.quantity}`).join(", ")
        )}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">{at(r.createTime)}</TableCell>
      <TableCell className="text-xs text-muted-foreground">{at(r.updateTime)}</TableCell>
    </TableRow>
  );
}
