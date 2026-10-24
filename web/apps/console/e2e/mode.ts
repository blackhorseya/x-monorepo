// Mirrors playwright.config.ts: with E2E_DATABASE_URL the server has Postgres,
// so admin RPCs work; without it they answer admin.no_database.
export const withDB = Boolean(process.env.E2E_DATABASE_URL);
