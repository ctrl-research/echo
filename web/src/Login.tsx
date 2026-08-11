import { useEffect, useState, type FormEvent } from "react";
import { useAuth, type Provider } from "./auth";

/**
 * Reads the failure reason the OAuth callback redirected back with.
 *
 * The callback cannot render an error itself — the user is mid-redirect in
 * their browser, and a bare 403 body would strand them with no way back — so it
 * sends them here with the reason in the query string.
 */
function useCallbackError(): string | null {
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    if (!params.get("error")) return;

    setError(params.get("error_description") || "Sign-in failed.");
    // Strip the parameters so a refresh does not resurrect a stale error.
    window.history.replaceState({}, "", window.location.pathname);
  }, []);

  return error;
}

export default function Login() {
  const { providers } = useAuth();
  const callbackError = useCallbackError();

  if (providers === undefined) {
    return <main className="centered">Loading…</main>;
  }

  const sso = providers.filter((p) => p.key !== "local");
  const local = providers.find((p) => p.key === "local");

  return (
    <main className="centered">
      <div className="card">
        <h1>Echo</h1>
        <p className="muted">Sign in to your library.</p>

        {callbackError && (
          <p className="error" role="alert">
            {callbackError}
          </p>
        )}

        {sso.map((p) => (
          <SSOButton key={p.key} provider={p} />
        ))}

        {sso.length > 0 && local && <div className="divider">or</div>}

        {local && <LocalLoginForm />}

        {providers.length === 0 && (
          <p className="error" role="alert">
            No sign-in method is configured. Set <code>ECHO_GOOGLE_CLIENT_ID</code>{" "}
            and <code>ECHO_GOOGLE_CLIENT_SECRET</code>, the{" "}
            <code>ECHO_OIDC_*</code> variables, or{" "}
            <code>ECHO_LOCAL_AUTH=true</code> on the server.
          </p>
        )}
      </div>
    </main>
  );
}

function SSOButton({ provider }: { provider: Provider }) {
  // A plain link, not fetch: the OAuth flow is a browser navigation, and the
  // identity provider needs to render its own consent screen.
  return (
    <a className="button sso" href={provider.startUrl}>
      {provider.key === "google" ? <GoogleMark /> : <span aria-hidden>🔑</span>}
      Sign in with {provider.name}
    </a>
  );
}

function LocalLoginForm() {
  const { login } = useAuth();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      setError(await login(email, password));
    } catch {
      setError("Could not reach the server");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={onSubmit}>
      <label htmlFor="email">Email</label>
      <input
        id="email"
        name="email"
        type="email"
        autoComplete="username"
        required
        value={email}
        onChange={(e) => setEmail(e.target.value)}
      />

      <label htmlFor="password">Password</label>
      <input
        id="password"
        name="password"
        type="password"
        autoComplete="current-password"
        required
        value={password}
        onChange={(e) => setPassword(e.target.value)}
      />

      {/* aria-live so the failure is announced, not just displayed. */}
      <p className="error" role="alert" aria-live="polite">
        {error ?? ""}
      </p>

      <button type="submit" disabled={busy}>
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}

function GoogleMark() {
  return (
    <svg width="18" height="18" viewBox="0 0 18 18" aria-hidden focusable="false">
      <path
        fill="#4285F4"
        d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62Z"
      />
      <path
        fill="#34A853"
        d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.34A9 9 0 0 0 9 18Z"
      />
      <path
        fill="#FBBC05"
        d="M3.97 10.72a5.4 5.4 0 0 1 0-3.44V4.94H.96a9 9 0 0 0 0 8.12l3.01-2.34Z"
      />
      <path
        fill="#EA4335"
        d="M9 3.58c1.32 0 2.5.46 3.44 1.35l2.58-2.58C13.46.9 11.43 0 9 0A9 9 0 0 0 .96 4.94l3.01 2.34C4.68 5.16 6.66 3.58 9 3.58Z"
      />
    </svg>
  );
}
