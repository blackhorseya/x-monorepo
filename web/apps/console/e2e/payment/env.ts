import { paymentPorts } from "../ports.ts";

export { paymentPorts };

// The "payment" Playwright project (docs/payment.md §9 S6, 「Console 真流程」): the whole payment stack on real
// processes and a real Postgres of its own, nothing scripted with page.route. Everything below is shared by the
// stack wrapper (stack-server.ts) and the specs, so the two cannot disagree on a port, a key or the database.
// The keys are made up for this test; none of them is a credential.

export const SHOPPER_KEY = "e2e-pay-shopper-key-0123456789abcdef";
export const ADMIN_KEY = "e2e-pay-admin-key-0123456789abcdef";
export const CATALOG_KEY = "e2e-pay-catalog-service-key-0123456789";
export const PAYMENT_KEY = "e2e-pay-payment-service-key-0123456789";
export const WEBHOOK_SECRET = "e2e-pay-webhook-secret-0123456789abcdef";
/** The customer the shopper key is: rpc takes the order's customer from the credential's name. */
export const SHOPPER = "shopper";

const bin = "../../../bin";
export const BIN = {
  migrate: `${bin}/migrate`,
  rpc: `${bin}/rpc`,
  worker: `${bin}/worker`,
  catalog: `${bin}/catalog-rpc`,
  payment: `${bin}/payment-rpc`,
};

/** The database of this stack: a name of its own on the server E2E_DATABASE_URL points at. */
export function databaseUrls(adminUrl: string, rpcPort: number): { admin: string; stack: string; name: string } {
  const name = `e2e_payment_${rpcPort}`;
  const stack = new URL(adminUrl);
  stack.pathname = `/${name}`;
  return { admin: adminUrl, stack: stack.toString(), name };
}

/** The environment of a payment-rpc/rpc/worker process: nothing inherited from APP_*, only what is set here. */
export function cleanEnv(extra: Record<string, string>): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(process.env)) if (!k.startsWith("APP_")) env[k] = v;
  return { ...env, APP_CONFIG: "/dev/null", APP_LOG_LEVEL: "warn", ...extra };
}

/** What `payment-rpc mock ...` needs: the same database and the same signing secret the stack runs on. */
export function mockEnv(stackUrl: string): NodeJS.ProcessEnv {
  return cleanEnv({
    APP_POSTGRES_URL: stackUrl,
    APP_PAYMENT_PROVIDER: "mock",
    APP_PAYMENT_MOCK_CONTROL: "true",
    APP_PAYMENT_MOCK_WEBHOOK_SECRET: WEBHOOK_SECRET,
  });
}
