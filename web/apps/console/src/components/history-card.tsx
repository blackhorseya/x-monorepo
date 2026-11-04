import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  OrderHistoryActorKind,
  OrderHistoryCause,
  OrderStatus,
  type Order,
  type OrderHistoryEntry,
} from "@repo/proto/ordering/v1/entity_pb";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useCredentialGeneration } from "../auth";
import { describe, isNotFound } from "../client";
import { historyQuery } from "../queries";
import { statusVariant } from "./order-status";

// Why an order moved, as a person reads it. Fixed sentences per cause: the server sends an enumeration, never
// text of another service. A cause this build does not know (a newer server) and UNSPECIFIED are the same
// honest "Unknown cause", not a blank and not the raw number.
const CAUSE_LABELS: Partial<Record<OrderHistoryCause, string>> = {
  [OrderHistoryCause.RPC_REQUEST]: "Requested by a client",
  [OrderHistoryCause.PAYMENT_CAPTURED]: "Payment received",
  [OrderHistoryCause.PAYMENT_EXPIRED]: "Payment window closed",
  [OrderHistoryCause.STOCK_RESERVED]: "Stock reserved",
  [OrderHistoryCause.CANCELLATION_REQUESTED]: "Cancellation requested (reason not classified)",
  [OrderHistoryCause.STOCK_SHORTAGE]: "Not enough stock",
  [OrderHistoryCause.RESERVATION_EXPIRED]: "Reservation expired",
};
export const causeLabel = (c: OrderHistoryCause): string => CAUSE_LABELS[c] ?? "Unknown cause";

const ACTOR_LABELS: Partial<Record<OrderHistoryActorKind, string>> = {
  [OrderHistoryActorKind.CUSTOMER]: "Customer",
  [OrderHistoryActorKind.ADMIN]: "Admin",
  [OrderHistoryActorKind.SYSTEM]: "System",
  [OrderHistoryActorKind.ANONYMOUS]: "Anonymous",
};
export const actorLabel = (k: OrderHistoryActorKind): string => ACTOR_LABELS[k] ?? "Unknown";

// What happened, named by the state it arrived in; the first row is the order coming to exist.
export const eventLabel = (e: OrderHistoryEntry): string =>
  e.fromStatus === OrderStatus.UNSPECIFIED ? "CREATED" : (OrderStatus[e.toStatus] ?? "UNKNOWN");

const transition = (e: OrderHistoryEntry): string =>
  e.fromStatus === OrderStatus.UNSPECIFIED
    ? (OrderStatus[e.toStatus] ?? "UNKNOWN")
    : `${OrderStatus[e.fromStatus] ?? "UNKNOWN"} → ${OrderStatus[e.toStatus] ?? "UNKNOWN"}`;

const at = (t?: { seconds: bigint }) => (t ? new Date(Number(t.seconds) * 1000).toLocaleString() : "");

// The History card of /orders/$id: every transition of the order, oldest first (seq), from GetOrderHistory.
//
// Whose eyes this is for is the server's call, not the card's: actor_subject comes back only for an admin and
// empty for everyone else, and the card shows a subject exactly when the answer carries one. What the card owns
// is that an answer never outlives the credential it was asked with. The query is keyed by the credential
// generation (queries.ts historyQuery) and the page gives the card that generation as its key, so after a switch
// or a sign-out the previous credential's rows, error and subjects are not in the entry the card reads, and a
// reply that arrives late is written under its own key.
//
// The history grows when the order moves, so the card follows the order's own poll (historyInterval) and asks once
// more when the snapshot it holds was taken under another status than the order has now (the last row of a final
// order arrives that way, even when the first answer was slow); the page's Cancel / Complete invalidate it too. An order from before the history existed has none: an empty state, not an error.
export function HistoryCard({ orderId, order }: { orderId: string; order: Order }) {
  const generation = useCredentialGeneration();
  const { data: snapshot, isError, error, isFetching, refetch } = useQuery(historyQuery(orderId, generation, order));
  const data = snapshot?.entries;

  // A snapshot taken while the order was in another status can lack the transitions since. Ask again once the
  // request in flight (if any) is done: a refetch cannot replace a first request that has no data yet, it would join
  // it and get the same stale answer. A final order is not polled, so this is the only thing that brings its last
  // row in when the first answer was slow. After the new answer asOf equals the order's status and it stops.
  //
  // A failed catch-up is shown and is not retried for the status it failed under (`tried`): that would loop. It does
  // not block the next status either: the order moving again is a new reason to ask, whatever happened before. So the
  // guard is "have I already tried for this status", not "is the card in an error state".
  const stale = snapshot !== undefined && snapshot.asOf !== order.status;
  const tried = useRef<OrderStatus | null>(null);
  useEffect(() => {
    if (!stale || isFetching || tried.current === order.status) return;
    tried.current = order.status;
    void refetch();
  }, [stale, isFetching, order.status, refetch]);

  const notFound = isError && isNotFound(error);
  return (
    <Card data-testid="history-card">
      <CardHeader>
        <CardTitle>History</CardTitle>
        <CardDescription>
          Every state change of this order, oldest first: what it moved from and to, who asked, and why.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {notFound && (
          <p data-testid="history-not-found" className="text-sm text-muted-foreground">
            No history to show: there is no such order for you.
          </p>
        )}
        {isError && !notFound && (
          <p data-testid="history-error" role="alert" className="text-sm text-destructive">
            The history could not be loaded: {describe(error)}
          </p>
        )}
        {data && data.length === 0 && (
          <p data-testid="history-empty" className="text-sm text-muted-foreground">
            No transitions recorded. An order created before order history was recorded has none; only changes
            made after that are listed.
          </p>
        )}
        {data && data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Event</TableHead>
                <TableHead>Transition</TableHead>
                <TableHead>When</TableHead>
                <TableHead>By</TableHead>
                <TableHead>Why</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.map((e) => (
                <TableRow key={e.seq} data-testid="history-row">
                  <TableCell>
                    <Badge variant={statusVariant(e.toStatus)} data-testid="history-event">
                      {eventLabel(e)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs" data-testid="history-transition">
                    {transition(e)}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground" data-testid="history-time">
                    {at(e.occurredTime)}
                  </TableCell>
                  <TableCell className="text-xs">
                    <span data-testid="history-actor">{actorLabel(e.actorKind)}</span>
                    {e.actorSubject !== "" && (
                      <>
                        {" "}
                        <code data-testid="history-subject">{e.actorSubject}</code>
                      </>
                    )}
                  </TableCell>
                  <TableCell className="text-xs" data-testid="history-cause">
                    {causeLabel(e.cause)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
