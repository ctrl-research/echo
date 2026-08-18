import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  AuthError,
  clearSession,
  loadSession,
  login,
  type Session,
} from "./session";

type SessionState = {
  session: Session | null;
  signIn: (
    server: string,
    username: string,
    password: string,
  ) => Promise<string | null>;
  signOut: () => void;
};

const SessionContext = createContext<SessionState | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  // Read synchronously rather than in an effect. The credential lives in
  // localStorage, not an HttpOnly cookie, so there is no round trip to wait on
  // and therefore no "loading" state to render before the first paint.
  //
  // A token revoked server-side still looks valid here; it surfaces as a 401 on
  // the first query, which is where it gets handled once data fetching lands.
  const [session, setSession] = useState<Session | null>(() => loadSession());

  const signIn = useCallback(
    async (
      server: string,
      username: string,
      password: string,
    ): Promise<string | null> => {
      try {
        setSession(await login(server, username, password));
        return null;
      } catch (err) {
        // A wrong password and an unreachable server are different problems
        // with different fixes, so they get different messages rather than one
        // catch-all.
        if (err instanceof AuthError) return err.message;
        return "Could not reach that server. Check the address and try again.";
      }
    },
    [],
  );

  const signOut = useCallback(() => {
    clearSession();
    setSession(null);
  }, []);

  const value = useMemo(
    () => ({ session, signIn, signOut }),
    [session, signIn, signOut],
  );
  return <SessionContext value={value}>{children}</SessionContext>;
}

export function useSession(): SessionState {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error("useSession must be used inside a SessionProvider");
  return ctx;
}
