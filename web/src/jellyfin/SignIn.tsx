import { useEffect, useState, type FormEvent } from "react";
import { useSession } from "./SessionProvider";
import QuickConnectForm from "./QuickConnectForm";
import { isEnabled } from "./quickconnect";

/**
 * Seeds the server field for the common case of one known server. It is only a
 * default — the field stays editable, because the same build may be installed
 * against a different server.
 */
const DEFAULT_SERVER = import.meta.env.VITE_JELLYFIN_URL ?? "";

type Method = "quick" | "password";

export default function SignIn() {
  const [server, setServer] = useState(DEFAULT_SERVER);
  const [method, setMethod] = useState<Method>("password");
  const [quickAvailable, setQuickAvailable] = useState(false);

  // Ask the server whether Quick Connect is on, and prefer it when it is: on a
  // phone, approving a code beats typing a password. Re-checked whenever the
  // address changes, since the answer belongs to the server, not the app.
  useEffect(() => {
    const address = server.trim();
    if (!address) {
      setQuickAvailable(false);
      return;
    }

    let cancelled = false;
    // Debounced: this runs per keystroke in the address field otherwise.
    const timer = setTimeout(() => {
      void isEnabled(address).then((enabled) => {
        if (cancelled) return;
        setQuickAvailable(enabled);
        if (enabled) setMethod("quick");
      });
    }, 500);

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [server]);

  return (
    <main className="centered">
      <div className="card">
        <h1>Echo</h1>

        <label>
          Server
          <input
            type="url"
            inputMode="url"
            placeholder="https://jellyfin.example.com"
            value={server}
            onChange={(e) => setServer(e.target.value)}
            required
            // Phone keyboards helpfully capitalise and autocorrect, which is
            // never right for a hostname or a username.
            autoCapitalize="none"
            autoCorrect="off"
          />
        </label>

        {quickAvailable && (
          <div className="tabs signin-methods" role="tablist">
            <button
              type="button"
              role="tab"
              aria-selected={method === "quick"}
              className={method === "quick" ? "active" : undefined}
              onClick={() => setMethod("quick")}
            >
              Quick Connect
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={method === "password"}
              className={method === "password" ? "active" : undefined}
              onClick={() => setMethod("password")}
            >
              Password
            </button>
          </div>
        )}

        {method === "quick" ? (
          <QuickConnectForm server={server.trim()} />
        ) : (
          <PasswordForm server={server} />
        )}
      </div>
    </main>
  );
}

function PasswordForm({ server }: { server: string }) {
  const { signIn } = useSession();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    // A blank password is valid: Jellyfin accounts can be created without one.
    const message = await signIn(server.trim(), username.trim(), password);
    setError(message);
    setBusy(false);
  }

  return (
    <form onSubmit={(e) => void onSubmit(e)}>
      <label>
        Username
        <input
          type="text"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          required
          autoCapitalize="none"
          autoCorrect="off"
          autoComplete="username"
        />
      </label>

      <label>
        Password
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
        />
      </label>

      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}

      <button type="submit" disabled={busy}>
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}
