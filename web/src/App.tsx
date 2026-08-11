import { useEffect, useState } from "react";
import { api } from "./api/client";
import { AuthProvider, useAuth } from "./auth";
import Login from "./Login";

type Health = {
  status: string;
  version: string;
  database: string;
};

export default function App() {
  return (
    <AuthProvider>
      <Root />
    </AuthProvider>
  );
}

function Root() {
  const { user } = useAuth();

  if (user === undefined) return <main className="centered">Loading…</main>;

  // A failed sign-in redirects to /signin?error=... . Show that even when a
  // session already exists — otherwise attempting to sign in as somebody else,
  // and being refused, silently drops you back into the previous account with
  // no explanation. There is no router until M4, so this is checked directly.
  const failed = new URLSearchParams(window.location.search).has("error");
  if (user === null || failed) return <Login />;

  return <Shell />;
}

/**
 * M1 placeholder: proves the session round trip and shows who is signed in.
 * Replaced by the library browser and player in M3/M4.
 */
function Shell() {
  const { user, logout } = useAuth();
  const [health, setHealth] = useState<Health | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.GET("/health").then(({ data }) => {
      if (!cancelled && data) setHealth(data as Health);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <main>
      <header className="bar">
        <h1>Echo</h1>
        <span>
          {user!.displayName || user!.email}
          {user!.role === "admin" && <span className="badge">admin</span>}
        </span>
        <button onClick={() => void logout()}>Sign out</button>
      </header>

      <p>Library browsing arrives in M3. The server is up and you are signed in.</p>

      {health && (
        <dl>
          <dt>Status</dt>
          <dd>{health.status}</dd>
          <dt>Database</dt>
          <dd>{health.database}</dd>
          <dt>Version</dt>
          <dd>{health.version}</dd>
        </dl>
      )}
    </main>
  );
}
