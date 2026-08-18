import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SessionProvider, useSession } from "./jellyfin/SessionProvider";
import SignIn from "./jellyfin/SignIn";
import Albums from "./library/Albums";

// A library changes when someone edits it on the server, which is rare and
// never urgent, so refetching on every window focus is pure noise on a phone.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 5 * 60_000, refetchOnWindowFocus: false, retry: 1 },
  },
});

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <SessionProvider>
        <Root />
      </SessionProvider>
    </QueryClientProvider>
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
        Playback, playlists, favourites, and history land on top of this shell.
        The previous player still targets the Go API and is kept as reference
        until it is rebuilt against Jellyfin.
      */}
      <main>
        <Albums />
      </main>
    </div>
  );
}
