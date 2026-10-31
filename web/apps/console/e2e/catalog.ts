import { expect, type APIRequestContext } from "@playwright/test";

// CreateOrder takes its prices from the catalog, so every SKU a spec orders has to be a product
// first. Seeded through the API (UpsertProduct needs the admin role when auth is on: pass the
// admin credential in `headers`), under a SKU of the spec's own so parallel specs never reprice
// each other's lines. 1299 minor USD is the price the specs' totals are written against.
export const PRICE_MINOR = 1299;

export async function seedProduct(
  request: APIRequestContext,
  sku: string,
  { amountMinor = PRICE_MINOR, currency = "USD", headers = {} }: { amountMinor?: number; currency?: string; headers?: Record<string, string> } = {},
) {
  const res = await request.post("/api/catalog.v1.CatalogService/UpsertProduct", {
    headers,
    data: { sku, name: sku, price: { amountMinor: String(amountMinor), currency } },
  });
  expect(res.ok(), `UpsertProduct ${sku}: ${res.status()} ${await res.text()}`).toBe(true);
}

// DeactivateProduct needs the admin role when auth is on, like UpsertProduct (`headers`).
export async function deactivateProduct(request: APIRequestContext, sku: string, headers: Record<string, string> = {}) {
  const res = await request.post("/api/catalog.v1.CatalogService/DeactivateProduct", { headers, data: { sku } });
  expect(res.ok(), `DeactivateProduct ${sku}: ${res.status()} ${await res.text()}`).toBe(true);
}
