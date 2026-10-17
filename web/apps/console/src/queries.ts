import { queryOptions } from "@tanstack/react-query";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { inventory, isNotFound, orders } from "./client";

// The server exposes unary RPCs only, so the console polls. Reservations are
// written by apps/worker after it consumes OrderCreated, and orders can be
// cancelled by inventory (shortfall), so both change without a user action.
const POLL_MS = 2000;

export const keys = {
  orders: ["orders"] as const,
  reservation: (orderId: string) => ["reservation", orderId] as const,
  stock: (sku: string) => ["stock", sku] as const,
};

export const ordersQuery = queryOptions({
  queryKey: keys.orders,
  queryFn: async () => (await orders.listOrders({ pageSize: 20 })).orders,
  refetchInterval: POLL_MS,
});

// null = the worker has not reserved anything for this order (yet).
export const reservationQuery = (orderId: string) =>
  queryOptions({
    queryKey: keys.reservation(orderId),
    queryFn: async () => {
      try {
        return (await inventory.getReservation({ orderId })).reservation ?? null;
      } catch (err) {
        if (isNotFound(err)) return null;
        throw err;
      }
    },
    // Everything but RESERVED is terminal; RESERVED can still be released and
    // a missing reservation can still appear.
    refetchInterval: (q) => {
      const s = q.state.data?.status;
      return s === undefined || s === ReservationStatus.RESERVED ? POLL_MS : false;
    },
  });

export const stockQuery = (sku: string) =>
  queryOptions({
    queryKey: keys.stock(sku),
    queryFn: async () => (await inventory.getStock({ sku })).stock ?? null,
    enabled: sku !== "",
  });
