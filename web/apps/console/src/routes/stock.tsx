import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { inventory } from "../client";
import { keys, stockQuery } from "../queries";
import { ErrorAlert, Field } from "./orders";

export function StockPage() {
  const qc = useQueryClient();
  const [sku, setSku] = useState("SKU-1");
  const [onHand, setOnHand] = useState(10);
  // The query follows the committed SKU, not every keystroke.
  const [lookup, setLookup] = useState("SKU-1");
  const stock = useQuery(stockQuery(lookup));

  const set = useMutation({
    mutationFn: () => inventory.setStock({ sku, onHand }),
    onSuccess: (res) => {
      qc.setQueryData(keys.stock(sku), res.stock ?? null);
      setLookup(sku);
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Stock</CardTitle>
        <CardDescription>On hand is set here; reserved changes as orders are created and cancelled.</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex flex-wrap items-end gap-2">
          <Field label="SKU" className="w-40">
            <Input value={sku} onChange={(e) => setSku(e.target.value)} />
          </Field>
          <Field label="On hand" className="w-28">
            <Input type="number" value={onHand} onChange={(e) => setOnHand(Number(e.target.value))} />
          </Field>
          <Button onClick={() => set.mutate()} disabled={set.isPending}>
            Set
          </Button>
          <Button variant="outline" onClick={() => (sku === lookup ? void stock.refetch() : setLookup(sku))}>
            Get
          </Button>
        </div>
        {set.isError && <ErrorAlert error={set.error} />}
        {stock.isError && <ErrorAlert error={stock.error} />}
        {stock.data && (
          <dl className="grid grid-cols-3 gap-4 sm:max-w-md">
            {[
              ["On hand", stock.data.onHand],
              ["Reserved", stock.data.reserved],
              ["Available", stock.data.available],
            ].map(([k, v]) => (
              <div key={k} className="rounded-lg border p-3">
                <dt className="text-xs text-muted-foreground">{k}</dt>
                <dd className="text-2xl font-semibold tabular-nums">{v}</dd>
              </div>
            ))}
          </dl>
        )}
      </CardContent>
    </Card>
  );
}
