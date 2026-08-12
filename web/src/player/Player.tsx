import { useEffect, useRef } from "react";
import { usePlayer, current, streamURL, artURL, formatTime } from "./store";

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
  const state = usePlayer();
  const track = current(state);

  // Load a new source only when the track actually changes. Assigning src on
  // every render would restart playback constantly.
  useEffect(() => {
    const el = audio.current;
    if (!el || !track) return;
    const wanted = streamURL(track.id);
    if (!el.src.endsWith(wanted)) {
      el.src = wanted;
      el.load();
    }
  }, [track?.id]);

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
  }, [state.playing, track?.id]);

  useEffect(() => {
    if (audio.current) audio.current.volume = state.volume;
  }, [state.volume]);

  // Lock-screen and hardware controls. Without this, a phone shows no metadata
  // and the headphone buttons do nothing.
  useEffect(() => {
    if (!("mediaSession" in navigator) || !track) return;

    navigator.mediaSession.metadata = new MediaMetadata({
      title: track.title,
      artist: track.artistName,
      album: track.albumName,
      artwork: track.coverArtId
        ? [{ src: artURL(track.coverArtId), sizes: "512x512" }]
        : undefined,
    });

    const { toggle, next, previous } = usePlayer.getState();
    navigator.mediaSession.setActionHandler("play", () => toggle());
    navigator.mediaSession.setActionHandler("pause", () => toggle());
    navigator.mediaSession.setActionHandler("nexttrack", () => next());
    navigator.mediaSession.setActionHandler("previoustrack", () => previous());
  }, [track?.id]);

  if (!track) return null;

  return (
    <footer className="player">
      <audio
        ref={audio}
        preload="metadata"
        onTimeUpdate={(e) => state._sync({ position: e.currentTarget.currentTime })}
        onDurationChange={(e) => state._sync({ duration: e.currentTarget.duration })}
        onEnded={() => state.next()}
        onPlay={() => state._sync({ playing: true })}
        onPause={() => state._sync({ playing: false })}
      />

      {track.coverArtId ? (
        <img className="player-art" src={artURL(track.coverArtId)} alt="" />
      ) : (
        <div className="player-art placeholder" aria-hidden />
      )}

      <div className="player-meta">
        <strong>{track.title}</strong>
        <span className="muted">
          {track.artistName}
          {track.albumName && ` — ${track.albumName}`}
        </span>
      </div>

      <div className="player-controls">
        <button onClick={state.previous} aria-label="Previous track">⏮</button>
        <button onClick={state.toggle} aria-label={state.playing ? "Pause" : "Play"}>
          {state.playing ? "⏸" : "▶"}
        </button>
        <button onClick={state.next} aria-label="Next track">⏭</button>
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
            // Seeking is a Range request under the hood; the element issues it
            // as soon as currentTime moves.
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
              state.repeat === "off" ? "all" : state.repeat === "all" ? "one" : "off",
            )
          }
          className={state.repeat !== "off" ? "active" : ""}
          aria-label={`Repeat: ${state.repeat}`}
        >
          {state.repeat === "one" ? "🔂" : "🔁"}
        </button>
      </div>
    </footer>
  );
}
