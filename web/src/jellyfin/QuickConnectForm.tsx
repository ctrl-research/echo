import { useEffect, useRef, useState } from "react";
import { useSession } from "./SessionProvider";
import { AuthError } from "./session";
import { initiate, waitForApproval } from "./quickconnect";

type Phase =
  | { kind: "idle" }
  | { kind: "starting" }
  | { kind: "waiting"; code: string }
  | { kind: "failed"; message: string };

export default function QuickConnectForm({ server }: { server: string }) {
  const { adopt } = useSession();
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });

  // Held in a ref rather than state: aborting is a side effect on unmount and
  // on restart, and it must reach the *current* poll loop without re-rendering
  // anything to do it.
  const abort = useRef<AbortController | null>(null);

  useEffect(() => () => abort.current?.abort(), []);

  // A code is only valid for the server it came from, so changing the address
  // mid-flow has to discard it rather than leave a stale code on screen.
  useEffect(() => {
    abort.current?.abort();
    setPhase({ kind: "idle" });
  }, [server]);

  async function start() {
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;

    setPhase({ kind: "starting" });
    try {
      const { code, secret } = await initiate(server);
      setPhase({ kind: "waiting", code });
      adopt(await waitForApproval(server, secret, controller.signal));
    } catch (err) {
      // An abort is this component tidying up after itself, not a failure the
      // user needs to see.
      if (err instanceof DOMException && err.name === "AbortError") return;
      setPhase({
        kind: "failed",
        message:
          err instanceof AuthError
            ? err.message
            : "Could not reach that server. Check the address and try again.",
      });
    }
  }

  if (phase.kind === "waiting") {
    return (
      <div className="quickconnect">
        <p className="code" aria-label={`Code ${phase.code.split("").join(" ")}`}>
          {phase.code}
        </p>
        <p>
          Enter this code in Jellyfin on a device you are already signed in on,
          under <strong>Settings → Quick Connect</strong>.
        </p>
        <p className="muted" role="status">
          Waiting for approval…
        </p>
        <button type="button" onClick={() => void start()}>
          Start again
        </button>
      </div>
    );
  }

  return (
    <div className="quickconnect">
      {phase.kind === "failed" && (
        <p className="error" role="alert">
          {phase.message}
        </p>
      )}
      <button
        type="button"
        onClick={() => void start()}
        disabled={phase.kind === "starting" || !server}
      >
        {phase.kind === "starting" ? "Getting a code…" : "Get a code"}
      </button>
    </div>
  );
}
