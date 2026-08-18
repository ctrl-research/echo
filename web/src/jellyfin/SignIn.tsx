import { useState, type FormEvent } from "react";
import { useSession } from "./SessionProvider";

/**
 * Seeds the server field for the common case of one known server. It is only a
 * default — the field stays editable, because the same build may be installed
 * against a different server.
 */
const DEFAULT_SERVER = import.meta.env.VITE_JELLYFIN_URL ?? "";

export default function SignIn() {
  const { signIn } = useSession();
  const [server, setServer] = useState(DEFAULT_SERVER);
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
    <main className="centered">
      <form className="signin" onSubmit={(e) => void onSubmit(e)}>
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
    </main>
  );
}
