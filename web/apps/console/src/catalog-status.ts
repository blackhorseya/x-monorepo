import { useQueries } from "@tanstack/react-query";
import { PRODUCT_STATUS_MAX, productStatusQuery } from "./queries";

// "checking" = no answer yet; "unknown" = the catalog could not be read, which says nothing about
// the SKU (never show it as "missing").
export type ProductStatus = "active" | "inactive" | "missing" | "checking" | "unknown";

// The catalog's answer for each SKU, in chunks of what one QuotePrices takes. Chunks follow the
// order given, so appending SKUs (a further page of the stock list) leaves the earlier chunks'
// queries as they were. A chunk whose latest read failed is "unknown" for all its SKUs, even if an
// earlier read of it succeeded: that answer may be stale.
export function useProductStatus(skus: string[]): (sku: string) => ProductStatus {
  const unique = [...new Set(skus)];
  const chunks: string[][] = [];
  for (let i = 0; i < unique.length; i += PRODUCT_STATUS_MAX) chunks.push(unique.slice(i, i + PRODUCT_STATUS_MAX));
  const results = useQueries({ queries: chunks.map((c) => productStatusQuery(c)) });
  const byChunk = new Map<string, (typeof results)[number]>();
  unique.forEach((sku, i) => byChunk.set(sku, results[Math.floor(i / PRODUCT_STATUS_MAX)]));
  return (sku) => {
    const q = byChunk.get(sku);
    if (q === undefined) return "checking";
    if (q.isError) return "unknown";
    if (q.data === undefined) return "checking";
    if (q.data.missing.has(sku)) return "missing";
    if (q.data.inactive.has(sku)) return "inactive";
    return "active";
  };
}
