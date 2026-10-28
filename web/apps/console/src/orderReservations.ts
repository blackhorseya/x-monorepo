import { useCallback, useEffect, useRef, useState } from "react";
import type { Reservation } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus, type Order } from "@repo/proto/ordering/v1/entity_pb";
import { useCredential } from "./auth";
import { inventory } from "./client";
import { POLL_MS, reservationPending } from "./queries";

// BatchGetReservations takes at most this many order ids (service.proto).
const BATCH_MAX = 100;

// What the list knows about one row: the reservation (null = none yet) and the order status it
// was fetched under. orderStatus moving is the cue to look again; lateSince is when the order
// turned terminal (ms epoch), where the wait for a reservation that is not there yet starts
// (see reservationPending).
export type RowReservation = { reservation: Reservation | null; orderStatus: OrderStatus; lateSince?: number };

const finished = (s: OrderStatus) => s === OrderStatus.COMPLETED || s === OrderStatus.CANCELLED;

// The rows to ask about now: never asked, asked under another order status, or still waiting
// for something (reservationPending). Everything else is settled and costs nothing.
export function wantedOrderIds(rows: readonly Order[], known: ReadonlyMap<string, RowReservation>, now: number): string[] {
  const ids: string[] = [];
  for (const o of rows) {
    const k = known.get(o.id);
    if (k === undefined || k.orderStatus !== o.status || reservationPending(o.status, k.reservation?.status, k.lateSince, now)) {
      ids.push(o.id);
    }
  }
  return ids;
}

// The reservation of every row of the orders list, in one BatchGetReservations per tick for
// the rows that are still waiting (none = no request), not a GetReservation per row. A tick
// also fires when the rows change (a new page, a status moving), and a hidden tab does not poll.
// Rows on pages that are not refreshed keep the order status they were loaded with, so a
// PENDING one there keeps being asked about until the page is reloaded.
export function useOrderReservations(rows: readonly Order[]) {
  const credential = useCredential();
  const [known, setKnown] = useState<ReadonlyMap<string, RowReservation>>(new Map());
  const [error, setError] = useState<unknown>();
  const latest = useRef({ rows, known });
  latest.current = { rows, known };
  const busy = useRef(false);
  const resyncAll = useRef(false); // a refocus asked for every row while a request was in flight
  const generation = useRef(0); // bumped when the credential changes: older answers are dropped

  // all: ask about every row, not only the waiting ones (a refocused tab).
  const refresh = useCallback(async (all = false) => {
    if (busy.current) {
      if (all) resyncAll.current = true; // not dropped: runs once the request in flight is done
      return;
    }
    const { rows, known } = latest.current;
    const ids = all ? rows.map((o) => o.id) : wantedOrderIds(rows, known, Date.now());
    if (ids.length === 0) return;
    busy.current = true;
    const gen = generation.current;
    const statuses = new Map(rows.map((o) => [o.id, o.status]));
    try {
      const found = new Map<string, Reservation>();
      const chunks: string[][] = [];
      for (let i = 0; i < ids.length; i += BATCH_MAX) chunks.push(ids.slice(i, i + BATCH_MAX));
      await Promise.all(
        chunks.map(async (orderIds) => {
          for (const r of (await inventory.batchGetReservations({ orderIds })).reservations) found.set(r.orderId, r);
        }),
      );
      if (gen !== generation.current) return;
      const now = Date.now();
      setKnown((prev) => {
        const next = new Map(prev);
        for (const id of ids) {
          const orderStatus = statuses.get(id) ?? OrderStatus.UNSPECIFIED;
          next.set(id, {
            reservation: found.get(id) ?? null,
            orderStatus,
            lateSince: finished(orderStatus) ? (prev.get(id)?.lateSince ?? now) : undefined,
          });
        }
        return next;
      });
      setError(undefined);
    } catch (err) {
      if (gen === generation.current) setError(err);
    } finally {
      if (gen === generation.current) {
        busy.current = false;
        if (resyncAll.current && document.visibilityState === "visible") {
          resyncAll.current = false;
          void refresh(true);
        }
      }
    }
  }, []);

  // New rows (a page, a filter) or an order whose status moved: look now, not at the next tick.
  const signature = rows.map((o) => `${o.id}:${o.status}`).join(",");
  useEffect(() => {
    void refresh();
  }, [signature, refresh]);

  useEffect(() => {
    const t = setInterval(() => {
      if (document.visibilityState === "visible") void refresh();
    }, POLL_MS);
    // Back on the tab: look at every row once, including the ones whose wait for a reservation ran
    // out (reservationPending) — the worker may have caught up meanwhile. Once, not a poll.
    const onVisible = () => {
      if (document.visibilityState === "visible") void refresh(true);
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [refresh]);

  // A credential change makes what was learned (and any error) about the old one void.
  const seenCredential = useRef(credential);
  useEffect(() => {
    if (seenCredential.current === credential) return;
    seenCredential.current = credential;
    generation.current++;
    busy.current = false;
    resyncAll.current = false;
    setKnown(new Map());
    setError(undefined);
  }, [credential]);

  return { known, error };
}
