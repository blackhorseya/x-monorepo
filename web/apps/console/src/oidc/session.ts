import { useSyncExternalStore } from "react";
import { getCredential, getCredentialGeneration, rotateSessionToken, setCredential, setSessionCredential } from "../auth";
import { getConfig } from "./config";
import { discover, type Endpoints } from "./discovery";
import { challengeFor, newVerifier, randomToken } from "./pkce";

// The console's OIDC session (docs/identity-login.md §8). The SPA runs Authorization Code + PKCE
// itself. Where each secret lives:
//   access token   memory only (src/auth.ts holds it as the bearer credential)
//   refresh token  sessionStorage, one key, rotated on every use
//   PKCE verifier, state, nonce   sessionStorage for the redirect round trip only; deleted the moment
//                  the callback reads them, whether the login then succeeds or not
// Nothing here is written to localStorage, logged, or put in a URL.

const SCOPE = "openid profile email offline_access"; // Dex only issues a refresh token for offline_access
const PENDING_KEY = "console.oidc.pending";
export const REFRESH_KEY = "console.oidc.refresh";
export const CALLBACK_PATH = "/auth/callback";

type Pending = { state: string; nonce: string; verifier: string; returnTo: string };

export type SessionState = {
  // The last login attempt failed: shown on /login and /auth/callback until the next attempt.
  error: string | null;
  // The refresh token is kept but the IdP could not be asked (network, 5xx, 429): the session is
  // not over, the console retries with backoff and /login offers a Retry.
  unreachable: boolean;
};

let state: SessionState = { error: null, unreachable: false };
const listeners = new Set<() => void>();
const emit = (next: Partial<SessionState>) => {
  state = { ...state, ...next };
  listeners.forEach((l) => l());
};
export const getSessionState = (): SessionState => state;
export function useSessionState(): SessionState {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    getSessionState,
  );
}

let endpoints: Endpoints | null = null;
async function endpointsFor(issuer: string): Promise<Endpoints> {
  return (endpoints ??= await discover(issuer));
}

const redirectUri = () => new URL(CALLBACK_PATH, window.location.origin).toString();

// Only a path inside this app is a place to come back to: not another origin, not the login pages.
export function safeReturnTo(raw: string | null | undefined): string {
  if (!raw || !raw.startsWith("/") || raw.startsWith("//") || raw.includes("\\")) return "/orders";
  if (raw.startsWith("/login") || raw.startsWith(CALLBACK_PATH)) return "/orders";
  return raw;
}

export async function startLogin(returnTo: string): Promise<void> {
  const oidc = getConfig().oidc;
  if (!oidc) return;
  emit({ error: null });
  try {
    const ep = await endpointsFor(oidc.issuer);
    const pending: Pending = {
      state: randomToken(24),
      nonce: randomToken(24),
      verifier: newVerifier(),
      returnTo: safeReturnTo(returnTo),
    };
    sessionStorage.setItem(PENDING_KEY, JSON.stringify(pending));
    const url = new URL(ep.authorization);
    url.searchParams.set("response_type", "code");
    url.searchParams.set("client_id", oidc.clientId);
    url.searchParams.set("redirect_uri", redirectUri());
    url.searchParams.set("scope", SCOPE);
    url.searchParams.set("state", pending.state);
    url.searchParams.set("nonce", pending.nonce);
    url.searchParams.set("code_challenge", await challengeFor(pending.verifier));
    url.searchParams.set("code_challenge_method", "S256");
    window.location.assign(url.toString());
  } catch (err) {
    emit({ error: message(err) });
  }
}

// take reads and deletes the pending login in one step: whatever happens next, its verifier,
// state and nonce cannot be used a second time.
function take(): Pending | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY);
    sessionStorage.removeItem(PENDING_KEY);
    return raw ? (JSON.parse(raw) as Pending) : null;
  } catch {
    return null;
  }
}

// The callback page renders twice in development (StrictMode) and the code is single use, so the
// exchange is shared by everyone who asks for the same callback URL.
let completing: { key: string; promise: Promise<string> } | null = null;

export function completeLogin(search: string): Promise<string> {
  if (completing?.key === search) return completing.promise;
  // The code and state are single use and now spent: drop them from the address bar (and history).
  window.history.replaceState(window.history.state, "", CALLBACK_PATH);
  const promise = exchange(new URLSearchParams(search));
  completing = { key: search, promise };
  return promise;
}

async function exchange(params: URLSearchParams): Promise<string> {
  const pending = take();
  const fail = (m: string): never => {
    emit({ error: m });
    throw new Error(m);
  };
  const oidc = getConfig().oidc;
  if (!oidc) return fail("OIDC login is not configured");
  if (!pending) return fail("No login is in progress; start again from the sign-in page");
  if (params.get("state") !== pending.state) return fail("The sign-in response did not match this browser's request (state mismatch); start again");
  const idpError = params.get("error");
  if (idpError) return fail(`The identity provider refused the sign-in: ${idpError}`);
  const code = params.get("code");
  if (!code) return fail("The sign-in response carried no authorization code");

  const generation = getCredentialGeneration();
  try {
    const ep = await endpointsFor(oidc.issuer);
    const tokens = await tokenRequest(ep.token, {
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri(),
      client_id: oidc.clientId,
      code_verifier: pending.verifier,
    });
    checkIdToken(tokens.id_token, oidc, pending.nonce);
    // A credential was set or cleared while the exchange ran (another sign-in, a sign-out): the
    // answer is for a login that is no longer wanted.
    if (generation !== getCredentialGeneration()) throw new Error("sign-in superseded");
    accept(tokens);
    return pending.returnTo;
  } catch (err) {
    return fail(message(err));
  }
}

export type TokenResponse = { access_token: string; refresh_token?: string; id_token?: string; expires_in?: number };

export class TokenError extends Error {
  constructor(
    message: string,
    // The IdP answered and refused (the grant is dead) as opposed to not answering (network, 5xx).
    readonly rejected: boolean,
  ) {
    super(message);
  }
}

export async function tokenRequest(endpoint: string, form: Record<string, string>): Promise<TokenResponse> {
  let res: Response;
  try {
    res = await fetch(endpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" },
      body: new URLSearchParams(form),
      cache: "no-store",
      credentials: "omit",
    });
  } catch {
    throw new TokenError("the identity provider could not be reached", false);
  }
  if (!res.ok) {
    const rejected = res.status === 400 || res.status === 401 || res.status === 403;
    throw new TokenError(`the identity provider refused the request (HTTP ${res.status})`, rejected);
  }
  const body = (await res.json()) as Partial<TokenResponse>;
  if (typeof body.access_token !== "string" || body.access_token === "") throw new TokenError("the identity provider returned no access token", true);
  return body as TokenResponse;
}

// The id token came straight from the token endpoint over TLS, so (OIDC Core 3.1.3.7) its signature
// is not re-checked here; what is checked is that it answers *this* request: issuer, audience and
// the nonce we put in the authorization request. The server verifies the access token itself.
function checkIdToken(idToken: string | undefined, oidc: { issuer: string; clientId: string }, nonce: string): void {
  if (!idToken) throw new Error("the identity provider returned no id token");
  let claims: { iss?: string; aud?: string | string[]; nonce?: string };
  try {
    claims = JSON.parse(atob(idToken.split(".")[1].replace(/-/g, "+").replace(/_/g, "/")));
  } catch {
    throw new Error("the id token is not readable");
  }
  const aud = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
  if (claims.iss !== oidc.issuer || !aud.includes(oidc.clientId)) throw new Error("the id token is for another issuer or client");
  if (claims.nonce !== nonce) throw new Error("the id token nonce does not match this sign-in (replayed response?)");
}

const message = (err: unknown): string => (err instanceof Error ? err.message : "sign-in failed");

// ── Session lifetime ────────────────────────────────────────────────────────────────────────────
//
// Failure policy of a refresh (the part §8 left to this implementation):
//   - the IdP answered 400 / 401 / 403 (invalid_grant, revoked, expired, rotated away): the refresh
//     token is dead. Clear the session and send the user to /login. Retrying cannot help.
//   - no answer (network error), 5xx, 408, 429: the IdP is unwell, not the session. Keep the refresh
//     token and the current access token, say so, retry with backoff (5 s doubling to 60 s), and let
//     /login offer an explicit Retry. Signing the user out for an IdP outage would turn it into ours.
// Every response is checked against the credential generation first: a sign-out (or another
// sign-in) that happened while the request was out wins, and the response is thrown away.

let sessionActive = false; // is the credential an OIDC access token (as opposed to an API key)
let timer: ReturnType<typeof setTimeout> | undefined;
let backoff = 5_000;

// accept installs a token response: the access token to memory, the (rotated) refresh token to
// sessionStorage, and the next silent refresh.
function accept(tokens: TokenResponse): void {
  if (sessionActive && getCredential()?.kind === "bearer") rotateSessionToken(tokens.access_token);
  else setSessionCredential(tokens.access_token);
  sessionActive = true;
  if (tokens.refresh_token) sessionStorage.setItem(REFRESH_KEY, tokens.refresh_token);
  backoff = 5_000;
  schedule(Math.max(5, (tokens.expires_in ?? 300) - 60) * 1000);
  emit({ error: null, unreachable: false });
}

function schedule(ms: number): void {
  clearTimeout(timer);
  timer = setTimeout(() => void refreshNow(), ms);
}

export type RefreshResult = "ok" | "none" | "rejected" | "unreachable" | "superseded";

let refreshing: Promise<RefreshResult> | null = null;

// refreshNow trades the refresh token for new tokens. Concurrent callers (the timer, a 401 retry,
// the Retry button) share one request: a refresh token is single use.
export function refreshNow(): Promise<RefreshResult> {
  return (refreshing ??= doRefresh().finally(() => (refreshing = null)));
}

async function doRefresh(): Promise<RefreshResult> {
  const oidc = getConfig().oidc;
  const refreshToken = readRefreshToken();
  if (!oidc || !refreshToken) return "none";
  // An API key is the credential now (signInWithApiKey drops the session; this is the backstop):
  // an OIDC refresh must never replace it.
  if (getCredential()?.kind === "api_key") return "none";
  const generation = getCredentialGeneration();
  try {
    const ep = await endpointsFor(oidc.issuer);
    const tokens = await tokenRequest(ep.token, { grant_type: "refresh_token", refresh_token: refreshToken, client_id: oidc.clientId });
    if (generation !== getCredentialGeneration()) return "superseded";
    accept(tokens);
    return "ok";
  } catch (err) {
    if (generation !== getCredentialGeneration()) return "superseded";
    if (err instanceof TokenError && err.rejected) {
      endLocal();
      emit({ error: "Your session has ended. Sign in again." });
      return "rejected";
    }
    emit({ unreachable: true });
    schedule(backoff);
    backoff = Math.min(backoff * 2, 60_000);
    return "unreachable";
  }
}

function readRefreshToken(): string | null {
  try {
    return sessionStorage.getItem(REFRESH_KEY);
  } catch {
    return null;
  }
}

// Called once before the first render: a reload (refresh token in sessionStorage) is signed back in
// before any page asks the server anything.
export async function initSession(): Promise<void> {
  if (getConfig().oidc && readRefreshToken()) await refreshNow();
}

// Is the current credential an OIDC session whose access token a refresh could replace?
export const canRefresh = (): boolean => sessionActive && readRefreshToken() !== null;

// Drops everything local: the credential (any kind), both tokens, the timer, a half-done login.
function endLocal(): void {
  clearTimeout(timer);
  sessionActive = false;
  try {
    sessionStorage.removeItem(REFRESH_KEY);
    sessionStorage.removeItem(PENDING_KEY);
  } catch {
    // storage unavailable: nothing was kept there
  }
  setSessionCredential(null);
  emit({ unreachable: false });
}

// Choosing an API key ends any OIDC session (one credential at a time, §8): the refresh token goes
// with it, so neither the refresh timer nor a reload can put the session back over the key.
export function signInWithApiKey(value: string): void {
  endLocal();
  setCredential({ kind: "api_key", value });
}

// Sign out. Local state goes first and unconditionally; the IdP's own session is only touched when
// its discovery document names an end_session_endpoint (Dex has none, so there it stays local).
// The redirect carries client_id and post_logout_redirect_uri, not id_token_hint: no token in a URL.
export async function signOut(): Promise<void> {
  endLocal();
  emit({ error: null });
  const oidc = getConfig().oidc;
  if (!oidc) return;
  try {
    const ep = await endpointsFor(oidc.issuer);
    if (!ep.endSession) return;
    const url = new URL(ep.endSession);
    url.searchParams.set("client_id", oidc.clientId);
    url.searchParams.set("post_logout_redirect_uri", new URL("/login", window.location.origin).toString());
    window.location.assign(url.toString());
  } catch {
    // IdP unreachable: already signed out here
  }
}
