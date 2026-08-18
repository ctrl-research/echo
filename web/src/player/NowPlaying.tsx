import { useEffect, useState } from "react";
import { imageUrl } from "../jellyfin/client";
import { useSession } from "../jellyfin/SessionProvider";
import { formatTime, type Track } from "./store";

export type DisplayMode = "standard" | "vinyl";

const MODE_KEY = "echo.displayMode";

/**
 * Remembered rather than reset each session: which of the two a listener wants
 * is a taste that does not change between sittings, and having to pick it again
 * every time would make the vinyl mode a novelty rather than a setting.
 */
function loadMode(): DisplayMode {
  return localStorage.getItem(MODE_KEY) === "vinyl" ? "vinyl" : "standard";
}

export default function NowPlaying({
  track,
  playing,
  position,
  duration,
  onClose,
}: {
  track: Track;
  playing: boolean;
  position: number;
  duration: number;
  onClose: () => void;
}) {
  const { session } = useSession();
  const [mode, setMode] = useState<DisplayMode>(loadMode);

  useEffect(() => {
    localStorage.setItem(MODE_KEY, mode);
  }, [mode]);

  const art =
    session && track.Id && track.ImageTags?.["Primary"]
      ? imageUrl(session, track.Id, 600)
      : null;

  return (
    <section className="nowplaying">
      <button type="button" className="link" onClick={onClose}>
        ▾ Close
      </button>

      <div className={`display display-${mode}`}>
        {mode === "vinyl" ? (
          // The record keeps spinning through a pause in the sense that it
          // holds its angle: animation-play-state pauses in place rather than
          // resetting, so resuming continues from where it stopped instead of
          // snapping back to the top.
          <div className="vinyl" data-spinning={playing}>
            <div className="vinyl-disc">
              {art ? (
                <img className="vinyl-label" src={art} alt="" />
              ) : (
                <div className="vinyl-label placeholder" aria-hidden="true" />
              )}
              <div className="vinyl-spindle" aria-hidden="true" />
            </div>
          </div>
        ) : art ? (
          <img className="cover" src={art} alt="" />
        ) : (
          <div className="cover placeholder" aria-hidden="true" />
        )}
      </div>

      <h2>{track.Name}</h2>
      <p className="muted">
        {track.AlbumArtist}
        {track.Album && ` — ${track.Album}`}
      </p>

      <p className="muted tabular">
        {formatTime(position)} / {formatTime(duration)}
      </p>

      <div className="tabs player-modes" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={mode === "standard"}
          className={mode === "standard" ? "active" : undefined}
          onClick={() => setMode("standard")}
        >
          Album
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={mode === "vinyl"}
          className={mode === "vinyl" ? "active" : undefined}
          onClick={() => setMode("vinyl")}
        >
          Vinyl
        </button>
      </div>
    </section>
  );
}
