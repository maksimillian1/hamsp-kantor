import { z } from "zod";

// Platform mode: KANTOR runs behind the HAMSP platform, which signs users in
// with Zitadel (OIDC authorization code + PKCE, public client). The config
// is read at run time from /platform-config.json; without it KANTOR uses its
// own sign-in.

const platformConfigSchema = z.object({
  issuer: z.url(),
  clientId: z.string().min(1),
  // Zitadel project ID of the platform API; the API Gateway checks this audience.
  audience: z.string().min(1),
});

export type PlatformConfig = z.infer<typeof platformConfigSchema>;

let config: PlatformConfig | null = null;

export async function loadPlatformConfig(): Promise<void> {
  try {
    const response = await fetch("/platform-config.json", { cache: "no-store" });
    if (!response.ok) {
      return;
    }
    const parsed = platformConfigSchema.safeParse(await response.json());
    if (!parsed.success) {
      console.error("Invalid /platform-config.json:", parsed.error.flatten());
      return;
    }
    config = parsed.data;
  } catch {
    // No config file (or not JSON, e.g. the SPA fallback page): not platform mode.
  }
}

export function isPlatformMode(): boolean {
  return config !== null;
}

// The backend serves only these modules in platform mode (PLATFORM_MODE); keep the two lists in step.
const PLATFORM_PATH_PREFIXES = ["/operational/overview", "/operational/projects", "/admin/", "/profile", "/forbidden"];
const PLATFORM_EXCLUDED_PATHS = ["/admin/registration"];

/** Whether a page is available; in platform mode only the modules the backend serves are. */
export function isPathAvailable(path: string): boolean {
  if (!isPlatformMode()) {
    return true;
  }
  if (PLATFORM_EXCLUDED_PATHS.some((excluded) => path.startsWith(excluded))) {
    return false;
  }
  return PLATFORM_PATH_PREFIXES.some((prefix) => path === prefix || path.startsWith(prefix.endsWith("/") ? prefix : `${prefix}/`));
}

function requireConfig(): PlatformConfig {
  if (!config) {
    throw new Error("platform mode is not configured");
  }
  return config;
}

interface Discovery {
  authorization_endpoint: string;
  token_endpoint: string;
  end_session_endpoint?: string;
}

let discovery: Promise<Discovery> | null = null;

function getDiscovery(): Promise<Discovery> {
  if (!discovery) {
    const { issuer } = requireConfig();
    discovery = fetch(`${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`)
      .then((response) => {
        if (!response.ok) {
          throw new Error(`OIDC discovery failed: ${response.status}`);
        }
        return response.json() as Promise<Discovery>;
      })
      .catch((error: unknown) => {
        discovery = null;
        throw error;
      });
  }
  return discovery;
}

export interface PlatformTokens {
  access_token: string;
  expires_in: number;
  refresh_token?: string;
  id_token?: string;
}

// Refresh and ID tokens stay in sessionStorage (per tab, gone when the tab
// closes; Zitadel's own session signs the user back in silently).
const REFRESH_KEY = "kantor-platform-refresh";
const ID_TOKEN_KEY = "kantor-platform-id-token";
const PENDING_KEY = "kantor-platform-pending";

function storeTokens(tokens: PlatformTokens) {
  if (tokens.refresh_token) {
    sessionStorage.setItem(REFRESH_KEY, tokens.refresh_token);
  }
  if (tokens.id_token) {
    sessionStorage.setItem(ID_TOKEN_KEY, tokens.id_token);
  }
}

function clearTokens() {
  sessionStorage.removeItem(REFRESH_KEY);
  sessionStorage.removeItem(ID_TOKEN_KEY);
}

export const CALLBACK_PATH = "/auth/callback";

function redirectUri() {
  return `${window.location.origin}${CALLBACK_PATH}`;
}

function base64url(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function randomString(): string {
  return base64url(crypto.getRandomValues(new Uint8Array(32)));
}

async function challengeFor(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64url(new Uint8Array(digest));
}

function scopes(audience: string): string {
  return [
    "openid",
    "profile",
    "email",
    "offline_access",
    "urn:zitadel:iam:user:resourceowner",
    `urn:zitadel:iam:org:project:id:${audience}:aud`,
  ].join(" ");
}

function safeReturnTo(value: string | undefined): string {
  return value && value.startsWith("/") && !value.startsWith("//") ? value : "/";
}

export async function startPlatformLogin(returnTo?: string): Promise<never> {
  const { clientId, audience } = requireConfig();
  const { authorization_endpoint } = await getDiscovery();
  const verifier = randomString();
  const state = randomString();
  sessionStorage.setItem(PENDING_KEY, JSON.stringify({ verifier, state, returnTo: safeReturnTo(returnTo) }));

  const url = new URL(authorization_endpoint);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", clientId);
  url.searchParams.set("redirect_uri", redirectUri());
  url.searchParams.set("scope", scopes(audience));
  url.searchParams.set("state", state);
  url.searchParams.set("code_challenge", await challengeFor(verifier));
  url.searchParams.set("code_challenge_method", "S256");
  window.location.assign(url.toString());
  return new Promise<never>(() => {});
}

async function tokenRequest(body: Record<string, string>): Promise<PlatformTokens> {
  const { token_endpoint } = await getDiscovery();
  const response = await fetch(token_endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ client_id: requireConfig().clientId, ...body }),
  });
  if (!response.ok) {
    throw new Error(`token request failed: ${response.status}`);
  }
  const tokens = (await response.json()) as PlatformTokens;
  storeTokens(tokens);
  return tokens;
}

/** Exchanges the code on the callback URL; returns the tokens and where to go next. */
export async function completePlatformLogin(callbackUrl: string): Promise<{ tokens: PlatformTokens; returnTo: string }> {
  const raw = sessionStorage.getItem(PENDING_KEY);
  sessionStorage.removeItem(PENDING_KEY);
  const pending = raw ? (JSON.parse(raw) as { verifier: string; state: string; returnTo: string }) : null;
  const params = new URL(callbackUrl).searchParams;
  const code = params.get("code");
  if (!pending || !code || params.get("state") !== pending.state) {
    throw new Error("sign-in response does not match the request");
  }
  const tokens = await tokenRequest({
    grant_type: "authorization_code",
    code,
    redirect_uri: redirectUri(),
    code_verifier: pending.verifier,
  });
  return { tokens, returnTo: safeReturnTo(pending.returnTo) };
}

/** Renews the access token with the refresh token; throws when there is none or it was refused. */
export async function refreshPlatformTokens(): Promise<PlatformTokens> {
  const refreshToken = sessionStorage.getItem(REFRESH_KEY);
  if (!refreshToken) {
    throw new Error("no refresh token");
  }
  try {
    return await tokenRequest({ grant_type: "refresh_token", refresh_token: refreshToken });
  } catch (error) {
    clearTokens();
    throw error;
  }
}

export async function platformLogout(): Promise<never> {
  const idToken = sessionStorage.getItem(ID_TOKEN_KEY);
  clearTokens();
  const { end_session_endpoint } = await getDiscovery();
  if (!end_session_endpoint) {
    window.location.assign("/");
    return new Promise<never>(() => {});
  }
  const url = new URL(end_session_endpoint);
  url.searchParams.set("client_id", requireConfig().clientId);
  url.searchParams.set("post_logout_redirect_uri", `${window.location.origin}/`);
  if (idToken) {
    url.searchParams.set("id_token_hint", idToken);
  }
  window.location.assign(url.toString());
  return new Promise<never>(() => {});
}
