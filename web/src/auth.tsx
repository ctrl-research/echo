import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { api } from "./api/client";
import type { components } from "./api/schema";

export type User = components["schemas"]["UserDTO"];
export type Provider = components["schemas"]["Provider"];

type AuthState = {
  /** undefined while the initial session check is in flight. */
  user: User | null | undefined;
  /** undefined until the provider list has loaded. */
  providers: Provider[] | undefined;
  login: (email: string, password: string) => Promise<string | null>;
  logout: () => Promise<void>;
};

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null | undefined>(undefined);
  const [providers, setProviders] = useState<Provider[] | undefined>(undefined);

  // One /auth/me on mount decides whether to show the app or the sign-in page.
  // It also seeds the CSRF cookie, which the client needs before it can make
  // its first state-changing request.
  useEffect(() => {
    let cancelled = false;

    api
      .GET("/auth/me")
      .then(({ data, error }) => {
        if (cancelled) return;
        setUser(error ? null : (data as User));
      })
      .catch(() => {
        if (!cancelled) setUser(null);
      });

    api
      .GET("/auth/providers")
      .then(({ data }) => {
        if (!cancelled) setProviders(data?.providers ?? []);
      })
      .catch(() => {
        if (!cancelled) setProviders([]);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(
    async (email: string, password: string): Promise<string | null> => {
      const { data, error } = await api.POST("/auth/login", {
        body: { email, password },
      });
      if (error) {
        // The server deliberately returns one message for every failure mode,
        // so there is nothing more specific to show.
        return "Invalid email or password";
      }
      setUser(data as User);
      return null;
    },
    [],
  );

  const logout = useCallback(async () => {
    await api.POST("/auth/logout", {});
    setUser(null);
  }, []);

  const value = useMemo(
    () => ({ user, providers, login, logout }),
    [user, providers, login, logout],
  );
  return <AuthContext value={value}>{children}</AuthContext>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside an AuthProvider");
  return ctx;
}
