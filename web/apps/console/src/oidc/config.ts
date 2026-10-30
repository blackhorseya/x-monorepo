// Runtime configuration of the console, fetched once at start-up from /config.json (same origin).
// In the nginx image the file is rendered from the CONSOLE_* environment (deploy/config.json.template);
// vite serves public/config.json (OIDC off). Only public values live here: the issuer, the public
// client id and whether the API key entry exists. Nothing in it is a secret.
export type AppConfig = {
  // Absent: OIDC is off and the console behaves as before (credential control, no login page).
  oidc: { issuer: string; clientId: string } | null;
  // console.apiKeyLogin: is pasting an API key (break-glass) offered as a way in.
  apiKeyLogin: boolean;
};

const defaults: AppConfig = { oidc: null, apiKeyLogin: true };

let current: AppConfig = defaults;

export function getConfig(): AppConfig {
  return current;
}

// parseConfig is total: anything unusable means "OIDC off", never a half-configured login.
export function parseConfig(raw: unknown): AppConfig {
  if (typeof raw !== "object" || raw === null) return defaults;
  const r = raw as Record<string, unknown>;
  const issuer = typeof r.oidcIssuer === "string" ? r.oidcIssuer.trim() : "";
  const clientId = typeof r.oidcClientId === "string" ? r.oidcClientId.trim() : "";
  return {
    oidc: isHttpURL(issuer) && clientId !== "" ? { issuer, clientId } : null,
    apiKeyLogin: r.apiKeyLogin !== false,
  };
}

function isHttpURL(s: string): boolean {
  try {
    const u = new URL(s);
    return u.protocol === "https:" || u.protocol === "http:";
  } catch {
    return false;
  }
}

export async function loadConfig(fetcher: typeof fetch = fetch): Promise<AppConfig> {
  try {
    const res = await fetcher("/config.json", { cache: "no-store", credentials: "omit" });
    current = res.ok ? parseConfig(await res.json()) : defaults;
  } catch {
    current = defaults; // vite without the file, offline, not JSON: OIDC off
  }
  return current;
}
