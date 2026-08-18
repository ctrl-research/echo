/**
 * Quick Connect: sign in by typing a six-character code into an already
 * authenticated Jellyfin session, rather than a password into this one.
 *
 * The best fit for a phone by some distance — no password typed on a touch
 * keyboard, and nothing to paste out of a password manager into a browser that
 * may not have one. Entirely core API, so it needs no plugins.
 *
 * The shape of the flow: initiate to get a code and a secret, show the user the
 * code, poll with the secret until they have approved it elsewhere, then trade
 * the secret for an access token.
 */

import {
  authHeader,
  AuthError,
  normalizeServer,
  sessionFrom,
  type Session,
} from "./session";

/** How often to ask whether the code has been approved yet. */
const POLL_INTERVAL_MS = 3_000;

/**
 * Give up after five minutes. Jellyfin expires the code on its own schedule,
 * but an abandoned tab should stop polling regardless of what the server does.
 */
const TIMEOUT_MS = 5 * 60_000;

export type QuickConnectStart = {
  code: string;
  secret: string;
};

/** Whether the server has Quick Connect turned on. */
export async function isEnabled(server: string): Promise<boolean> {
  try {
    const res = await fetch(`${normalizeServer(server)}/QuickConnect/Enabled`, {
      headers: { Authorization: authHeader() },
    });
    if (!res.ok) return false;
    return (await res.json()) === true;
  } catch {
    // An unreachable server is not a Quick Connect problem; the password form
    // will surface it with a better message.
    return false;
  }
}

/** Begins a flow, returning the code to show the user. */
export async function initiate(server: string): Promise<QuickConnectStart> {
  const res = await fetch(`${normalizeServer(server)}/QuickConnect/Initiate`, {
    method: "POST",
    headers: { Authorization: authHeader() },
  });
  if (!res.ok) {
    throw new AuthError(
      res.status === 401
        ? "Quick Connect is not enabled on this server"
        : `Server returned ${res.status}`,
    );
  }

  const body = (await res.json()) as { Code?: string; Secret?: string };
  if (!body.Code || !body.Secret) {
    throw new AuthError("Server did not return a Quick Connect code");
  }
  return { code: body.Code, secret: body.Secret };
}

/**
 * Polls until the code is approved, then exchanges the secret for a session.
 *
 * `signal` matters more than it looks: without it a poll loop outlives the
 * component that started it, and a user who switches back to the password form
 * would leave a request firing every three seconds until the timeout.
 */
export async function waitForApproval(
  server: string,
  secret: string,
  signal: AbortSignal,
): Promise<Session> {
  const base = normalizeServer(server);
  const deadline = Date.now() + TIMEOUT_MS;

  while (Date.now() < deadline) {
    if (signal.aborted) throw new DOMException("Aborted", "AbortError");

    const res = await fetch(
      `${base}/QuickConnect/Connect?secret=${encodeURIComponent(secret)}`,
      { headers: { Authorization: authHeader() }, signal },
    );

    // 404 is the server saying it has forgotten this code — expired, or
    // revoked from the other device. Retrying cannot help.
    if (res.status === 404) {
      throw new AuthError("That code expired. Start again for a new one.");
    }
    if (!res.ok) throw new AuthError(`Server returned ${res.status}`);

    const body = (await res.json()) as { Authenticated?: boolean };
    if (body.Authenticated) return authenticate(base, secret);

    await delay(POLL_INTERVAL_MS, signal);
  }

  throw new AuthError("That code expired. Start again for a new one.");
}

/** Trades an approved secret for an access token. */
async function authenticate(server: string, secret: string): Promise<Session> {
  const res = await fetch(
    `${normalizeServer(server)}/Users/AuthenticateWithQuickConnect`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: authHeader(),
      },
      body: JSON.stringify({ Secret: secret }),
    },
  );
  if (!res.ok) throw new AuthError(`Server returned ${res.status}`);
  return sessionFrom(server, await res.json());
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    }
    signal.addEventListener("abort", onAbort, { once: true });
  });
}
