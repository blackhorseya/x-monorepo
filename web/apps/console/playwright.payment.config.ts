import { defineConfig, devices } from "@playwright/test";
import { paymentPorts } from "./e2e/ports";

// The "payment" project (docs/payment.md §9 S6, 「Console 真流程」): the console against the REAL payment stack —
// catalog-rpc, payment-rpc serve, payment-worker, rpc and worker, a Postgres database made for the run — with nothing
// scripted by page.route. `task web-e2e-payment`; needs E2E_DATABASE_URL (a Postgres server it may create a
// database on; CI's console-e2e job has one) and the built binaries and console (`task build web-build`).
//
// Its own config, not a project of playwright.config.ts: that file's worker (e2e/worker.setup.ts) consumes the
// events of the shared database with ordering.require_payment off, so a payment_required order could not be
// told from one whose snapshot it took. Here the stack is one webServer (e2e/payment/stack-server.ts), which owns
// every process and the database and is asked for SIGTERM (it cleans up), and vite preview is the console.
const ports = paymentPorts();

export default defineConfig({
  testDir: "e2e/payment",
  testMatch: /.*\.spec\.ts$/,
  fullyParallel: false,
  workers: 1, // one stack, one worker switch
  forbidOnly: !!process.env.CI,
  retries: 0, // a failure here is a real flow failing; a retry would only run it on the state it left
  reporter: process.env.CI ? [["list"], ["html", { open: "never", outputFolder: "playwright-report-payment" }]] : "list",
  use: { baseURL: `http://localhost:${ports.web}`, trace: "retain-on-failure" },
  projects: [{ name: "payment", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      command: "node --experimental-strip-types e2e/payment/stack-server.ts",
      url: `http://127.0.0.1:${ports.control}/ready`,
      timeout: 120_000,
      gracefulShutdown: { signal: "SIGTERM" as const, timeout: 30_000 },
      reuseExistingServer: false, // a stack someone else started is not the one this run's database belongs to
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `pnpm exec vite preview --port ${ports.web} --strictPort`,
      url: `http://localhost:${ports.web}/`,
      env: { CONSOLE_API: `http://127.0.0.1:${ports.rpc}` },
      reuseExistingServer: false,
    },
  ],
});
