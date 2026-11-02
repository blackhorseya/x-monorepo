import { useMutation, useQuery } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { ExternalLink } from "lucide-react";
import { useState } from "react";
import { PaymentStatus } from "@repo/proto/payment/v1/entity_pb";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getCredentialGeneration, useResetOnCredentialChange } from "../auth";
import { payments } from "../client";
import { paymentQuery } from "../queries";

// The checkout page address comes from the payment provider: data from outside. Only http(s) is ever
// opened or linked; a javascript:, data: or protocol-relative address is not a payment page.
export function safeCheckoutUrl(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

// What the user is told when the payment could not be started: a fixed sentence per x-error-code, never
// what the server wrote (docs/payment.md §7; the console shows no server text for a payment).
function messageFor(err: unknown): string {
  const e = ConnectError.from(err);
  switch (e.metadata.get("x-error-code")) {
    case "payment.disabled":
      return "Payment is not available for this order.";
    case "payment.not_pending":
      return "This payment can no longer be started.";
    case "payment.expired":
      return "The payment window for this order has closed.";
    case "payment.not_found":
    case "order.not_found":
      return "There is no payment for this order.";
  }
  if (e.code === Code.Unavailable) return "Payment could not be reached. Please try again.";
  if (e.code === Code.Unauthenticated) return "Sign in to pay for this order.";
  return "The payment could not be started. Please try again.";
}

// The Pay card of /orders/$id. The page shows it for one condition only: the order is CONFIRMED and its own
// payment_required snapshot is true. The console does not know the ordering.require_payment switch and does
// not ask: with the default configuration every snapshot is false and this card never appears. Pressing Pay
// asks rpc to start the payment (StartPayment takes the order id and nothing else: who pays is the
// credential's business) and opens the provider's checkout page in a new tab, without an opener; the page
// itself keeps watching the order (queries.ts orderInterval) so the customer comes back to the result.
export function PaymentCard({ orderId }: { orderId: string }) {
  const payment = useQuery(paymentQuery(orderId));
  const [message, setMessage] = useState<string | null>(null);
  const [startedUrl, setStartedUrl] = useState<string | null>(null);

  const start = useMutation({
    mutationFn: async () => {
      const generation = getCredentialGeneration();
      const res = await payments.startPayment({ orderId });
      return { payment: res.payment, generation };
    },
    // An answer for a credential that was replaced meanwhile belongs to the old one: it opens nothing
    // here (useResetOnCredentialChange resets the mutation's state, not a call in flight).
    onSuccess: ({ payment: started, generation }) => {
      if (generation !== getCredentialGeneration()) return;
      const url = safeCheckoutUrl(started?.checkoutUrl);
      if (url === null) {
        setStartedUrl(null);
        setMessage("The payment page address is not usable. Please try again.");
        return;
      }
      setMessage(null);
      setStartedUrl(url);
      window.open(url, "_blank", "noopener");
    },
    onError: (err) => setMessage(messageFor(err)),
  });
  useResetOnCredentialChange(start);

  const link = startedUrl ?? safeCheckoutUrl(payment.data?.checkoutUrl);
  const status = payment.data ? PaymentStatus[payment.data.status] : null;

  return (
    <Card data-testid="payment-card">
      <CardHeader>
        <CardTitle>Payment</CardTitle>
        <CardDescription>
          This order is waiting for its payment. Pay opens the payment page in a new tab; this page keeps watching the order and shows the
          result when you come back.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="flex flex-wrap items-center gap-3">
          <Button onClick={() => start.mutate()} disabled={start.isPending}>
            Pay
          </Button>
          {status && (
            <span data-testid="payment-status" className="text-xs text-muted-foreground">
              {status}
            </span>
          )}
        </div>
        {link && (
          <a
            data-testid="payment-link"
            href={link}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-sm underline"
          >
            Open the payment page <ExternalLink className="size-3" />
          </a>
        )}
        {message && (
          <p data-testid="payment-message" role="status" className="text-sm text-destructive">
            {message}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
