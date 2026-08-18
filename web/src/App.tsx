import { SessionProvider, useSession } from "./jellyfin/SessionProvider";
import SignIn from "./jellyfin/SignIn";

export default function App() {
  return (
    <SessionProvider>
      <Root />
    </SessionProvider>
  );
}

function Root() {
  const { session } = useSession();
  return session ? <Shell /> : <SignIn />;
}

function Shell() {
  const { session, signOut } = useSession();

  return (
    <div className="app">
      <header className="bar">
        <h1>Echo</h1>
        <span>{session!.userName}</span>
        <button onClick={signOut}>Sign out</button>
      </header>

      {/*
        Browse, playback, playlists, favourites, and history land on top of
        this shell. The previous library and player still target the Go API and
        are kept as reference until each is rebuilt against Jellyfin.
      */}
      <main className="centered">
        <p>Signed in to {session!.server}</p>
      </main>
    </div>
  );
}
