import { useEffect, useRef, useState } from "react";
import { audioUrl, imageUrl } from "../jellyfin/client";
import { useSession } from "../jellyfin/SessionProvider";
import NowPlaying from "./NowPlaying";
import { reportProgress, reportStart, reportStopped } from "./playstate";
import { current, formatTime, usePlayer } from "./store";

/** How often to tell Jellyfin where we are. Its own clients use ten seconds. */
const PROGRESS_INTERVAL_MS = 10_000;

/**
 * The player.
 *
 * One <audio> element for the lifetime of the app, deliberately. iOS only
 * reliably permits playback on an element whose first play() came from a user
 * gesture, so creating a fresh element per track breaks advancing to the next
 * one — the classic symptom being audio that works on desktop and silently
 * stops after one track on a phone.
 */
export default function Player() {
  const audio = useRef<HTMLAudioElement>(null);
  const { session } = useSession();
  const state = usePlayer();
  const track = current(state);
  const [expanded, setExpanded] = useState(false);

  // Load a new source only when the track actually changes. Assigning src on
  // every render would restart playback constantly.
  useEffect(() => {
    const el = audio.current;
    if (!el || !track?.Id || !session) return;
    const wanted = audioUrl(session, track.Id);
    if (el.src !== wanted) {
      el.src = wanted;
      el.load();
    }
  }, [track?.Id, session]);

  useEffect(() => {
    const el = audio.current;
    if (!el || !track) return;
    if (state.playing) {
      // Rejected when no user gesture has happened yet; reflect that in the UI
      // rather than leaving a play button that looks stuck.
      el.play().catch(() => usePlayer.getState()._sync({ playing: false }));
    } else {
      el.pause();
    }
  }, [state.playing, track?.Id]);

  useEffect(() => {
    if (audio.current) audio.current.volume = state.volume;
  }, [state.volume]);

  // Lock-screen and hardware controls. Without this, a phone shows no metadata
  // and the headphone buttons do nothing.
  useEffect(() => {
    if (!("mediaSession" in navigator) || !track || !session) return;

    const art =
      track.Id && track.ImageTags?.["Primary"]
        ? [{ src: imageUrl(session, track.Id, 512), sizes: "512x512" }]
        : undefined;

    navigator.mediaSession.metadata = new MediaMetadata({
      title: track.Name ?? "",
      artist: track.AlbumArtist ?? "",
      album: track.Album ?? "",
      artwork: art,
    });

    const { toggle, next, previous } = usePlayer.getState();
    navigator.mediaSession.setActionHandler("play", () => toggle());
    navigator.mediaSession.setActionHandler("pause", () => toggle());
    navigator.mediaSession.setActionHandler("nexttrack", () => next());
    navigator.mediaSession.setActionHandler("previoustrack", () => previous());
  }, [track?.Id, session]);

  // Playstate reporting. Start on a new track, progress on a timer, and stopped
  // when the track changes or the player goes away — Jellyfin needs the last of
  // those to close the session out, otherwise the dashboard shows this client
  // playing something forever.
  useEffect(() => {
    const id = track?.Id;
    if (!session || !id) return;

    void reportStart(session, id);
    const timer = setInterval(() => {
      const s = usePlayer.getState();
      void reportProgress(session, id, s.position, !s.playing);
    }, PROGRESS_INTERVAL_MS);

    return () => {
      clearInterval(timer);
      void reportStopped(session, id, usePlayer.getState().position);
    };
  }, [track?.Id, session]);

  if (!track) return null;

  const art =
    session && track.Id && track.ImageTags?.["Primary"]
      ? imageUrl(session, track.Id, 96)
      : null;

  return (
    <>
      {expanded && (
        <NowPlaying
          track={track}
          playing={state.playing}
          position={state.position}
          duration={state.duration}
          onClose={() => setExpanded(false)}
        />
      )}

      <footer className="player">
        <audio
          ref={audio}
          preload="metadata"
          onTimeUpdate={(e) =>
            state._sync({ position: e.currentTarget.currentTime })
          }
          onDurationChange={(e) =>
            state._sync({ duration: e.currentTarget.duration })
          }
          onEnded={() => state.next()}
          onPlay={() => state._sync({ playing: true })}
          onPause={() => state._sync({ playing: false })}
        />

        <button
          type="button"
          className="player-art-button"
          onClick={() => setExpanded(true)}
          aria-label="Open now playing"
        >
          {art ? (
            <img className="player-art" src={art} alt="" />
          ) : (
            <div className="player-art placeholder" aria-hidden="true" />
          )}
        </button>

        <div className="player-meta">
          <strong>{track.Name}</strong>
          <span className="muted">
            {track.AlbumArtist}
            {track.Album && ` — ${track.Album}`}
          </span>
        </div>

        <div className="player-controls">
          <button onClick={state.previous} aria-label="Previous track">
            ⏮
          </button>
          <button
            onClick={state.toggle}
            aria-label={state.playing ? "Pause" : "Play"}
          >
            {state.playing ? "⏸" : "▶"}
          </button>
          <button onClick={state.next} aria-label="Next track">
            ⏭
          </button>
        </div>

        <div className="player-scrubber">
          <span className="tabular">{formatTime(state.position)}</span>
          <input
            type="range"
            min={0}
            max={state.duration || 0}
            step={0.5}
            value={state.position}
            aria-label="Seek"
            onChange={(e) => {
              const to = Number(e.target.value);
              // Seeking is a Range request under the hood; the element issues
              // it as soon as currentTime moves.
              if (audio.current) audio.current.currentTime = to;
              state._sync({ position: to });
            }}
          />
          <span className="tabular">{formatTime(state.duration)}</span>
        </div>

        <div className="player-modes">
          <button
            onClick={state.toggleShuffle}
            aria-pressed={state.shuffle}
            className={state.shuffle ? "active" : ""}
            aria-label="Shuffle"
          >
            🔀
          </button>
          <button
            onClick={() =>
              state.setRepeat(
                state.repeat === "off"
                  ? "all"
                  : state.repeat === "all"
                    ? "one"
                    : "off",
              )
            }
            className={state.repeat !== "off" ? "active" : ""}
            aria-label={`Repeat: ${state.repeat}`}
          >
            {state.repeat === "one" ? "🔂" : "🔁"}
          </button>
        </div>
      </footer>
    </>
  );
}
