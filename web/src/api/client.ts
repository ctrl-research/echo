import createClient from "openapi-fetch";
import type { paths } from "./schema";

/** Methods that never need a CSRF token. Mirrors isSafeMethod on the server. */
const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS", "TRACE"]);

const CSRF_COOKIE = "echo_csrf";
const CSRF_HEADER = "X-CSRF-Token";

function readCookie(name: string): string | null {
  const prefix = `${name}=`;
  for (const part of document.cookie.split("; ")) {
    if (part.startsWith(prefix)) {
      return decodeURIComponent(part.slice(prefix.length));
    }
  }
  return null;
}

/**
 * Typed API client.
 *
 * `paths` is generated from the server's OpenAPI document by `make types`;
 * nothing here is hand-maintained, so a handler signature change on the Go
 * side becomes a TypeScript error rather than a runtime surprise.
 */
export const api = createClient<paths>({
  baseUrl: "/api/v1",
  // The session is an HttpOnly cookie, so every request must carry
  // credentials. Same-origin suffices: the client is served by the same host
  // in production, and Vite proxies /api in development to preserve that.
  credentials: "same-origin",
});

/**
 * Attaches the CSRF token to state-changing requests.
 *
 * The token lives in a readable (non-HttpOnly) cookie precisely so it can be
 * echoed back in a header — that asymmetry is what the double-submit pattern
 * relies on, since a cross-site attacker can cause the cookie to be sent but
 * cannot read it to set the header.
 */
api.use({
  onRequest({ request }) {
    if (SAFE_METHODS.has(request.method)) return request;

    const token = readCookie(CSRF_COOKIE);
    if (token) request.headers.set(CSRF_HEADER, token);
    return request;
  },
});
