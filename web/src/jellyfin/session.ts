/**
 * Connection and credentials for a Jellyfin server.
 *
 * The client is a static PWA that may be served from anywhere, so the server
 * address is runtime state rather than a build-time constant: the user names
 * their server at sign-in and it is remembered from then on. VITE_JELLYFIN_URL
 * only seeds the field for the common case of a single known server.
 */

const STORE_KEY = "echo.session";
const DEVICE_KEY = "echo.deviceId";

/**
 * Sent to the server as the client name and version; these label the session
 * in Jellyfin's dashboard and are not load-bearing, so a plain constant is
 * enough until there is a release process to stamp it from.
 */
export const CLIENT_NAME = "Echo";
export const CLIENT_VERSION = "0.1.0";

export type Session = {
  /** Origin of the Jellyfin server, no trailing slash. */
  server: string;
  token: string;
  userId: string;
  userName: string;
};

/**
 * A stable per-install identifier.
 *
 * Jellyfin keys sessions on this, so regenerating it every load would leave a
 * trail of dead sessions in the dashboard and break "remote control this
 * device". Persisted rather than derived: there is nothing stable about a
 * browser to derive it from.
 */
export function deviceId(): string {
  let id = localStorage.getItem(DEVICE_KEY);
  if (!id) {
    id = crypto.randomUUID();
    localStorage.setItem(DEVICE_KEY, id);
  }
  return id;
}

export function loadSession(): Session | null {
  const raw = localStorage.getItem(STORE_KEY);
  if (!raw) return null;
  try {
    const s = JSON.parse(raw) as Session;
    return s.server && s.token && s.userId ? s : null;
  } catch {
    // A corrupt entry is not worth crashing the app over; treat it as signed
    // out and let the user authenticate again.
    return null;
  }
}

export function saveSession(s: Session): void {
  localStorage.setItem(STORE_KEY, JSON.stringify(s));
}

export function clearSession(): void {
  localStorage.removeItem(STORE_KEY);
}

/**
 * The MediaBrowser authorization header every request carries.
 *
 * Jellyfin takes the token in this structured header rather than as a bearer
 * value, and uses the other fields to label the session. Token is omitted
 * before sign-in — the authenticate call needs the same header, minus the
 * credential it is about to obtain.
 */
export function authHeader(token?: string): string {
  const parts = [
    `Client="${CLIENT_NAME}"`,
    `Device="${navigator.platform || "Browser"}"`,
    `DeviceId="${deviceId()}"`,
    `Version="${CLIENT_VERSION}"`,
  ];
  if (token) parts.push(`Token="${token}"`);
  return `MediaBrowser ${parts.join(", ")}`;
}

export class AuthError extends Error {}

/**
 * Exchanges a username and password for an access token.
 *
 * Deliberately a plain fetch rather than the generated client: this is the one
 * call that happens before a server and token exist, so it cannot depend on a
 * client configured from them.
 */
export async function login(
  server: string,
  username: string,
  password: string,
): Promise<Session> {
  const base = server.replace(/\/+$/, "");
  const res = await fetch(`${base}/Users/AuthenticateByName`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: authHeader(),
    },
    body: JSON.stringify({ Username: username, Pw: password }),
  });

  if (res.status === 401) throw new AuthError("Incorrect username or password");
  if (!res.ok) throw new AuthError(`Server returned ${res.status}`);

  const body = (await res.json()) as {
    AccessToken?: string;
    User?: { Id?: string; Name?: string };
  };
  if (!body.AccessToken || !body.User?.Id) {
    throw new AuthError("Server did not return a usable session");
  }

  const session: Session = {
    server: base,
    token: body.AccessToken,
    userId: body.User.Id,
    userName: body.User.Name ?? username,
  };
  saveSession(session);
  return session;
}
