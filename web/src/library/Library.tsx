import { useEffect, useState } from "react";
import { api } from "../api/client";
import { usePlayer, artURL, formatTime } from "../player/store";
import type { Track } from "../player/store";
import type { components } from "../api/schema";

type Album = components["schemas"]["AlbumDTO"];
type Artist = components["schemas"]["ArtistDTO"];
type Genre = components["schemas"]["GenreDTO"];

type Tab = "albums" | "artists" | "tracks";

export default function Library() {
  const [tab, setTab] = useState<Tab>("albums");
  const [query, setQuery] = useState("");
  const [genre, setGenre] = useState("");
  const [genres, setGenres] = useState<Genre[]>([]);

  useEffect(() => {
    api.GET("/genres").then(({ data }) => setGenres(data?.genres ?? []));
  }, []);

  return (
    <div className="library">
      <div className="toolbar">
        <input
          type="search"
          placeholder="Search…"
          value={query}
          aria-label="Search the library"
          onChange={(e) => setQuery(e.target.value)}
        />
        <select
          value={genre}
          aria-label="Filter by genre"
          onChange={(e) => setGenre(e.target.value)}
        >
          <option value="">All genres</option>
          {genres.map((g) => (
            <option key={g.id} value={g.name}>
              {g.name} ({g.trackCount})
            </option>
          ))}
        </select>
        <nav className="tabs">
          {(["albums", "artists", "tracks"] as Tab[]).map((t) => (
            <button
              key={t}
              className={tab === t ? "active" : ""}
              onClick={() => setTab(t)}
              aria-pressed={tab === t}
            >
              {t}
            </button>
          ))}
        </nav>
      </div>

      {query ? (
        <SearchResults query={query} />
      ) : tab === "albums" ? (
        <Albums genre={genre} />
      ) : tab === "artists" ? (
        <Artists />
      ) : (
        <Tracks genre={genre} />
      )}
    </div>
  );
}

// ---- search ------------------------------------------------------------------

function SearchResults({ query }: { query: string }) {
  const [tracks, setTracks] = useState<Track[]>([]);
  const [artists, setArtists] = useState<Artist[]>([]);
  const [fuzzy, setFuzzy] = useState(false);

  useEffect(() => {
    // Debounced: a request per keystroke would be one per character typed.
    const timer = setTimeout(() => {
      api.GET("/search", { params: { query: { q: query } } }).then(({ data }) => {
        setTracks((data?.tracks ?? []) as Track[]);
        setArtists(data?.artists ?? []);
        setFuzzy(data?.fuzzy ?? false);
      });
    }, 200);
    return () => clearTimeout(timer);
  }, [query]);

  return (
    <div>
      {fuzzy && <p className="muted">No exact matches — showing close ones.</p>}
      {artists.length > 0 && (
        <p className="muted">
          Artists: {artists.map((a) => a.name).join(", ")}
        </p>
      )}
      <TrackList tracks={tracks} emptyMessage={`Nothing matches “${query}”.`} />
    </div>
  );
}

// ---- albums ------------------------------------------------------------------

function Albums({ genre }: { genre: string }) {
  const [albums, setAlbums] = useState<Album[]>([]);
  const [open, setOpen] = useState<Album | null>(null);

  useEffect(() => {
    const query = genre ? { genre } : {};
    api.GET("/albums", { params: { query } }).then(({ data }) => setAlbums(data?.albums ?? []));
  }, [genre]);

  if (open) return <AlbumDetail album={open} onBack={() => setOpen(null)} />;

  if (albums.length === 0) return <p className="muted">No albums yet.</p>;

  return (
    <ul className="grid">
      {albums.map((album) => (
        <li key={album.id}>
          <button className="card-button" onClick={() => setOpen(album)}>
            {album.coverArtId ? (
              <img src={artURL(album.coverArtId)} alt="" loading="lazy" />
            ) : (
              <div className="placeholder" aria-hidden />
            )}
            <strong>{album.name}</strong>
            <span className="muted">{album.artistName}</span>
            <span className="muted">
              {album.trackCount} track{album.trackCount === 1 ? "" : "s"}
              {album.year ? ` · ${album.year}` : ""}
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

function AlbumDetail({ album, onBack }: { album: Album; onBack: () => void }) {
  const [tracks, setTracks] = useState<Track[]>([]);

  useEffect(() => {
    api
      .GET("/albums/{id}", { params: { path: { id: album.id } } })
      .then(({ data }) => setTracks((data?.tracks ?? []) as Track[]));
  }, [album.id]);

  return (
    <div>
      <button className="link" onClick={onBack}>
        ← Albums
      </button>
      <header className="album-header">
        {album.coverArtId ? (
          <img src={artURL(album.coverArtId)} alt="" />
        ) : (
          <div className="placeholder" aria-hidden />
        )}
        <div>
          <h2>{album.name}</h2>
          <p className="muted">
            {album.artistName}
            {album.year ? ` · ${album.year}` : ""}
          </p>
        </div>
      </header>
      <TrackList tracks={tracks} showTrackNumbers emptyMessage="No tracks." />
    </div>
  );
}

// ---- artists -----------------------------------------------------------------

function Artists() {
  const [artists, setArtists] = useState<Artist[]>([]);
  const [open, setOpen] = useState<Artist | null>(null);
  const [tracks, setTracks] = useState<Track[]>([]);

  useEffect(() => {
    api.GET("/artists").then(({ data }) => setArtists(data?.artists ?? []));
  }, []);

  useEffect(() => {
    if (!open) return;
    api
      .GET("/tracks", { params: { query: { artist: open.id } } })
      .then(({ data }) => setTracks((data?.tracks ?? []) as Track[]));
  }, [open?.id]);

  if (open) {
    return (
      <div>
        <button className="link" onClick={() => setOpen(null)}>
          ← Artists
        </button>
        <h2>{open.name}</h2>
        <TrackList tracks={tracks} emptyMessage="No tracks." />
      </div>
    );
  }

  return (
    <ul className="rows">
      {artists.map((artist) => (
        <li key={artist.id}>
          <button className="row-button" onClick={() => setOpen(artist)}>
            <span>{artist.name}</span>
            <span className="muted">
              {artist.trackCount} track{artist.trackCount === 1 ? "" : "s"} ·{" "}
              {artist.albumCount} album{artist.albumCount === 1 ? "" : "s"}
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

// ---- tracks ------------------------------------------------------------------

function Tracks({ genre }: { genre: string }) {
  const [tracks, setTracks] = useState<Track[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [more, setMore] = useState(false);

  useEffect(() => {
    setTracks([]);
    setCursor(undefined);
    const query: Record<string, string> = genre ? { genre } : {};
    api.GET("/tracks", { params: { query } }).then(({ data }) => {
      setTracks((data?.tracks ?? []) as Track[]);
      setCursor(data?.nextCursor);
      setMore(Boolean(data?.nextCursor));
    });
  }, [genre]);

  function loadMore() {
    const query: Record<string, string> = genre ? { genre } : {};
    if (cursor) query.cursor = cursor;
    api.GET("/tracks", { params: { query } }).then(({ data }) => {
      setTracks((prev) => [...prev, ...((data?.tracks ?? []) as Track[])]);
      setCursor(data?.nextCursor);
      setMore(Boolean(data?.nextCursor));
    });
  }

  return (
    <div>
      <TrackList tracks={tracks} emptyMessage="No tracks yet — try a scan." />
      {more && (
        <button className="link" onClick={loadMore}>
          Load more
        </button>
      )}
    </div>
  );
}

// ---- shared ------------------------------------------------------------------

function TrackList({
  tracks,
  emptyMessage,
  showTrackNumbers = false,
}: {
  tracks: Track[];
  emptyMessage: string;
  showTrackNumbers?: boolean;
}) {
  const playQueue = usePlayer((s) => s.playQueue);
  const playingId = usePlayer((s) => s.queue[s.index]?.id);

  if (tracks.length === 0) return <p className="muted">{emptyMessage}</p>;

  return (
    <ol className="tracks">
      {tracks.map((track, i) => (
        <li key={track.id} className={track.id === playingId ? "playing" : ""}>
          {/* Clicking a track queues the whole list from that point, which is
              what every music player does and what makes an album playable. */}
          <button className="row-button" onClick={() => playQueue(tracks, i)}>
            <span className="tabular num">
              {showTrackNumbers ? (track.trackNo ?? i + 1) : i + 1}
            </span>
            <span className="title">{track.title}</span>
            <span className="muted">{track.artistName}</span>
            <span className="muted tabular">
              {track.durationMs ? formatTime(track.durationMs / 1000) : ""}
            </span>
          </button>
        </li>
      ))}
    </ol>
  );
}
