import { AuthProvider, useAuth } from "./auth";
import Login from "./Login";
import Library from "./library/Library";
import Player from "./player/Player";

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

function Shell() {
  const { user, logout } = useAuth();

  return (
    <div className="app">
      <header className="bar">
        <h1>Echo</h1>
        <span>
          {user!.displayName || user!.email}
          {user!.role === "admin" && <span className="badge">admin</span>}
        </span>
        <button onClick={() => void logout()}>Sign out</button>
      </header>

      <Library />
      <Player />
    </div>
  );
}
