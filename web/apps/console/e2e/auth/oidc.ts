import { createHash } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { mintJWT } from "./jwt";

// A stand-in identity provider for the console's OIDC login: every URL of it is answered by
// page.route(), so no IdP runs and the cross-origin fetches (discovery, token) never leave the
// browser. The access tokens it hands out are HS256 JWTs the auth project's real rpc accepts
// (mintJWT), so a signed-in console talks to the real server.

export const ISSUER = "https://idp.e2e.example/dex";
export const CLIENT_ID = "x-console-e2e";
export const AUTHORIZE = `${ISSUER}/auth`;
export const TOKEN = `${ISSUER}/token`;
export const END_SESSION = `${ISSUER}/logout`;

export type Idp = {
  /** Query parameters of every authorization request the console made, in order. */
  authorizeRequests: URLSearchParams[];
  /** Form bodies of every token request, in order. */
  tokenRequests: URLSearchParams[];
  /** Refresh tokens the IdP will accept; a refresh with another one is invalid_grant. */
  validRefreshTokens: Set<string>;
  /** Next token request fails with this HTTP status (and no body) instead of being answered. */
  failTokenWith: number | null;
  /** Like failTokenWith, but only for the refresh_token grant. */
  failRefreshWith: number | null;
  /** Access token lifetime in seconds. */
  expiresIn: number;
  /** When set (even to null), sent as expires_in instead of `expiresIn`: the IdP being wrong or hostile. */
  expiresInRaw?: unknown;
  /** Claims for the access token (roles etc.). */
  claims: Record<string, unknown>;
  /** Claims for the next access tokens, one entry per issue; once empty, `claims` applies. */
  claimsQueue: Record<string, unknown>[];
  /** While set, refresh_token grants wait for it before being answered. */
  gate: Promise<void> | null;
  /** Answer the next authorization request with a state the console never sent (a forged callback). */
  tamperState: boolean;
  accessTokens: string[];
  idTokens: string[];
  refreshTokens: string[];
};

export type IdpOptions = {
  /** The issuer to serve (default ISSUER). */
  issuer?: string;
  /** token_endpoint to advertise in discovery (default <issuer>/token). */
  tokenEndpoint?: string;
  endSession?: boolean;
  claims?: Record<string, unknown>;
  expiresIn?: number;
};

const cors = {
  "access-control-allow-origin": "*",
  "access-control-allow-headers": "*",
  "access-control-allow-methods": "GET, POST, OPTIONS",
};

const json = (route: Route, body: unknown, status = 200) =>
  route.fulfill({ status, headers: { ...cors, "content-type": "application/json" }, body: JSON.stringify(body) });

const b64url = (s: string) => Buffer.from(s).toString("base64url");
const challengeOf = (verifier: string) => createHash("sha256").update(verifier).digest("base64url");

/** What the console reads at start-up to know whether OIDC is on (runtime config, /config.json). */
export async function stubConfig(page: Page, config: { oidc?: boolean; apiKeyLogin?: boolean; issuer?: string; clientId?: string } = {}) {
  const { oidc = true, apiKeyLogin = true, issuer = ISSUER, clientId = CLIENT_ID } = config;
  await page.route("**/config.json", (route) =>
    route.fulfill({
      status: 200,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
      body: JSON.stringify({
        ...(oidc ? { oidcIssuer: issuer, oidcClientId: clientId } : {}),
        apiKeyLogin,
      }),
    }),
  );
}

/** Installs the IdP routes on the page and returns its recorded state. */
export async function mockIdp(page: Page, options: IdpOptions = {}): Promise<Idp> {
  const idp: Idp = {
    authorizeRequests: [],
    tokenRequests: [],
    validRefreshTokens: new Set(),
    failTokenWith: null,
    failRefreshWith: null,
    expiresIn: options.expiresIn ?? 300,
    claims: options.claims ?? {},
    claimsQueue: [],
    gate: null,
    tamperState: false,
    accessTokens: [],
    idTokens: [],
    refreshTokens: [],
  };
  const issuer = options.issuer ?? ISSUER;
  const authorize = `${issuer}/auth`;
  const tokenUrl = options.tokenEndpoint ?? `${issuer}/token`;
  const endSessionUrl = `${issuer}/logout`;
  let seq = 0;
  // code → { challenge, nonce } of the authorization request that issued it.
  const codes = new Map<string, { challenge: string; nonce: string }>();

  await page.route(`${issuer}/.well-known/openid-configuration`, (route) =>
    json(route, {
      issuer,
      authorization_endpoint: authorize,
      token_endpoint: tokenUrl,
      ...(options.endSession ? { end_session_endpoint: endSessionUrl } : {}),
    }),
  );

  await page.route(`${authorize}?**`, (route) => {
    const q = new URL(route.request().url()).searchParams;
    idp.authorizeRequests.push(q);
    const code = `code-${++seq}`;
    codes.set(code, { challenge: q.get("code_challenge") ?? "", nonce: q.get("nonce") ?? "" });
    const back = new URL(q.get("redirect_uri") ?? "");
    back.searchParams.set("code", code);
    back.searchParams.set("state", idp.tamperState ? "forged-state" : (q.get("state") ?? ""));
    return route.fulfill({ status: 302, headers: { location: back.toString() } });
  });

  await page.route(endSessionUrl + "**", (route) =>
    route.fulfill({ status: 200, headers: { "content-type": "text/html" }, body: "<title>idp logged out</title>" }),
  );

  const issue = (nonce: string) => {
    const access = mintJWT("e2e-oidc-user", { ...(idp.claimsQueue.shift() ?? idp.claims) });
    const refresh = `refresh-${++seq}`;
    idp.validRefreshTokens.add(refresh);
    const idToken = `${b64url(JSON.stringify({ alg: "none" }))}.${b64url(JSON.stringify({ iss: issuer, aud: CLIENT_ID, sub: "e2e-oidc-user", nonce }))}.`;
    idp.accessTokens.push(access);
    idp.idTokens.push(idToken);
    idp.refreshTokens.push(refresh);
    return {
      token_type: "Bearer",
      access_token: access,
      id_token: idToken,
      refresh_token: refresh,
      expires_in: "expiresInRaw" in idp ? idp.expiresInRaw : idp.expiresIn,
    };
  };

  await page.route(tokenUrl, async (route) => {
    if (route.request().method() === "OPTIONS") return route.fulfill({ status: 204, headers: cors });
    const form = new URLSearchParams(route.request().postData() ?? "");
    idp.tokenRequests.push(form);
    const grant = form.get("grant_type");
    if (idp.failTokenWith) return route.fulfill({ status: idp.failTokenWith, headers: cors });
    if (grant === "authorization_code") {
      const issued = codes.get(form.get("code") ?? "");
      codes.delete(form.get("code") ?? "");
      const verifier = form.get("code_verifier") ?? "";
      if (!issued || issued.challenge !== challengeOf(verifier) || form.get("client_id") !== CLIENT_ID) {
        return json(route, { error: "invalid_grant" }, 400);
      }
      return json(route, issue(issued.nonce));
    }
    if (grant === "refresh_token") {
      if (idp.gate) await idp.gate;
      if (idp.failRefreshWith) return route.fulfill({ status: idp.failRefreshWith, headers: cors });
      const presented = form.get("refresh_token") ?? "";
      if (!idp.validRefreshTokens.delete(presented)) return json(route, { error: "invalid_grant" }, 400);
      return json(route, issue(""));
    }
    return json(route, { error: "unsupported_grant_type" }, 400);
  });

  return idp;
}

/** Every string the page keeps in Web Storage (local + session), for "no token in there" checks. */
export async function webStorage(page: Page): Promise<string> {
  return page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
}

/** Records everything the page logs to its console. */
export function collectConsole(page: Page): string[] {
  const lines: string[] = [];
  page.on("console", (m) => lines.push(m.text()));
  page.on("pageerror", (e) => lines.push(String(e)));
  return lines;
}

/** Clicks the sign-in button on /login and waits until the console is back signed in. */
export async function signIn(page: Page) {
  await page.getByRole("button", { name: "Sign in" }).click();
}
