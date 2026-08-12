import { useEffect, useState } from "react";
import { api } from "../api/client";
import ConfirmDialog from "./ConfirmDialog";
import { usePlayer, artURL, formatTime } from "../player/store";
import type { Track } from "../player/store";
import type { components } from "../api/schema";

type Album = components["schemas"]["AlbumDTO"];
type Playlist = components["schemas"]["PlaylistDTO"];
type PlaylistEntry = components["schemas"]["PlaylistEntryDTO"];
type Artist = components["schemas"]["ArtistDTO"];
type Genre = components["schemas"]["GenreDTO"];

type Tab = "albums" | "artists" | "tracks" | "playlists" | "favourites";

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
          {(["albums", "artists", "tracks", "playlists", "favourites"] as Tab[]).map((t) => (
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
      ) : tab === "playlists" ? (
        <Playlists />
      ) : tab === "favourites" ? (
        <Favourites />
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
      <TrackList tracks={tracks} listId={`search:${query}`} emptyMessage={`Nothing matches “${query}”.`} />
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
      <TrackList tracks={tracks} listId={`album:${album.id}`} showTrackNumbers emptyMessage="No tracks." />
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
        <TrackList tracks={tracks} listId={`artist:${open.id}`} emptyMessage="No tracks." />
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
      <TrackList tracks={tracks} listId="tracks" emptyMessage="No tracks yet — try a scan." />
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
  listId,
  showTrackNumbers = false,
  onRemove,
}: {
  tracks: Track[];
  emptyMessage: string;
  /** Identifies this list so the player can tell it apart from any other. */
  listId: string;
  showTrackNumbers?: boolean;
  /** Present only for playlist entries, keyed by entry id rather than track. */
  onRemove?: (index: number) => void;
}) {
  const playQueue = usePlayer((s) => s.playQueue);
  const playingId = usePlayer((s) => s.queue[s.index]?.id);
  const playingIndex = usePlayer((s) => s.index);
  const queueId = usePlayer((s) => s.queueId);

  // When this list is the queue, the highlight follows the position — a
  // playlist may hold the same song twice and only the copy being played
  // should light up. Viewing some other list falls back to matching by id,
  // which still shows what is playing without pretending to know which row.
  const isPlayingRow = (track: Track, i: number) =>
    queueId === listId ? i === playingIndex : track.id === playingId;
  // Optimistic local state: the heart should flip on click, not on round trip.
  const [favourites, setFavourites] = useState<Record<string, boolean>>({});

  if (tracks.length === 0) return <p className="muted">{emptyMessage}</p>;

  const isFavourite = (t: Track) => favourites[t.id] ?? t.favorite;

  async function toggleFavourite(track: Track) {
    const next = !isFavourite(track);
    setFavourites((f) => ({ ...f, [track.id]: next }));
    const path = "/favorites/{type}/{id}" as const;
    const params = { params: { path: { type: "track" as const, id: track.id } } };
    const { error } = next ? await api.PUT(path, params) : await api.DELETE(path, params);
    if (error) setFavourites((f) => ({ ...f, [track.id]: !next }));
  }

  return (
    <ol className="tracks">
      {tracks.map((track, i) => (
        <li key={track.id + ":" + i} className={isPlayingRow(track, i) ? "playing" : ""}>
          {/* Clicking a track queues the whole list from that point, which is
              what every music player does and what makes an album playable. */}
          <button className="row-button" onClick={() => playQueue(tracks, i, listId)}>
            <span className="tabular num">
              {showTrackNumbers ? (track.trackNo ?? i + 1) : i + 1}
            </span>
            <span className="title">{track.title}</span>
            <span className="muted">{track.artistName}</span>
            <span className="muted tabular">
              {track.durationMs ? formatTime(track.durationMs / 1000) : ""}
            </span>
          </button>
          <button
            className="icon"
            aria-pressed={isFavourite(track)}
            aria-label={isFavourite(track) ? "Remove favourite" : "Add favourite"}
            onClick={() => void toggleFavourite(track)}
          >
            {isFavourite(track) ? "♥" : "♡"}
          </button>
          <AddToPlaylist track={track} />
          {onRemove && (
            <button className="icon" aria-label="Remove from playlist" onClick={() => onRemove(i)}>
              ✕
            </button>
          )}
        </li>
      ))}
    </ol>
  );
}

function AddToPlaylist({ track }: { track: Track }) {
  const [open, setOpen] = useState(false);
  const [playlists, setPlaylists] = useState<Playlist[]>([]);
  // Set when the server refuses a duplicate; holds what to retry on confirm.
  const [duplicate, setDuplicate] = useState<Playlist | null>(null);

  async function load() {
    const { data } = await api.GET("/playlists");
    setPlaylists((data?.playlists ?? []).filter((p) => p.owned));
    setOpen(true);
  }

  async function add(playlist: Playlist, allowDuplicate = false) {
    const { error, response } = await api.POST("/playlists/{id}/tracks", {
      params: { path: { id: playlist.id } },
      body: { trackId: track.id, ...(allowDuplicate ? { allowDuplicate: true } : {}) },
    });
    // 409 is the server saying the track is already there. Ask, rather than
    // silently duplicating or silently refusing.
    if (error && response?.status === 409) {
      setDuplicate(playlist);
      return;
    }
    setDuplicate(null);
  }

  return (
    <>
      {open ? (
        <select
          aria-label="Choose a playlist"
          defaultValue=""
          onChange={async (e) => {
            const chosen = playlists.find((p) => p.id === e.target.value);
            setOpen(false);
            if (chosen) await add(chosen);
          }}
          onBlur={() => setOpen(false)}
        >
          <option value="">Add to…</option>
          {playlists.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
      ) : (
        <button className="icon" aria-label="Add to playlist" onClick={() => void load()}>
          ＋
        </button>
      )}

      <ConfirmDialog
        open={duplicate !== null}
        title="Already in this playlist"
        message={`“${track.title}” is already in ${duplicate?.name ?? "this playlist"}. Add it again?`}
        confirmLabel="Add anyway"
        onConfirm={() => {
          const target = duplicate;
          setDuplicate(null);
          if (target) void add(target, true);
        }}
        onCancel={() => setDuplicate(null)}
      />
    </>
  );
}

// ---- playlists ------------------------------------------------------------------

function Playlists() {
  const [playlists, setPlaylists] = useState<Playlist[]>([]);
  const [open, setOpen] = useState<Playlist | null>(null);
  const [name, setName] = useState("");

  async function refresh() {
    const { data } = await api.GET("/playlists");
    setPlaylists(data?.playlists ?? []);
  }
  useEffect(() => {
    void refresh();
  }, []);

  if (open) return <PlaylistDetail playlist={open} onBack={() => { setOpen(null); void refresh(); }} />;

  return (
    <div>
      <form
        className="inline-form"
        onSubmit={async (e) => {
          e.preventDefault();
          if (!name.trim()) return;
          await api.POST("/playlists", { body: { name: name.trim() } });
          setName("");
          void refresh();
        }}
      >
        <input
          value={name}
          placeholder="New playlist…"
          aria-label="New playlist name"
          onChange={(e) => setName(e.target.value)}
        />
        <button type="submit">Create</button>
      </form>

      {playlists.length === 0 ? (
        <p className="muted">No playlists yet.</p>
      ) : (
        <ul className="rows">
          {playlists.map((p) => (
            <li key={p.id}>
              <button className="row-button" onClick={() => setOpen(p)}>
                <span className="title">{p.name}</span>
                <span className="muted">
                  {p.trackCount} track{p.trackCount === 1 ? "" : "s"}
                  {p.owned ? "" : ` · shared by ${p.ownerName}`}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function PlaylistDetail({ playlist, onBack }: { playlist: Playlist; onBack: () => void }) {
  const [entries, setEntries] = useState<PlaylistEntry[]>([]);

  async function refresh() {
    const { data } = await api.GET("/playlists/{id}", {
      params: { path: { id: playlist.id } },
    });
    setEntries((data?.tracks ?? []) as PlaylistEntry[]);
  }
  useEffect(() => {
    void refresh();
  }, [playlist.id]);

  const tracks = entries.map((e) => e.track as Track);

  return (
    <div>
      <button className="link" onClick={onBack}>
        ← Playlists
      </button>
      <h2>{playlist.name}</h2>
      {!playlist.owned && <p className="muted">Shared by {playlist.ownerName}</p>}
      <TrackList
        tracks={tracks}
        listId={`playlist:${playlist.id}`}
        emptyMessage="Nothing here yet — add tracks with ＋."
        onRemove={
          playlist.owned
            ? async (i) => {
                await api.DELETE("/playlists/{id}/tracks/{entryId}", {
                  params: { path: { id: playlist.id, entryId: entries[i].entryId } },
                });
                void refresh();
              }
            : undefined
        }
      />
    </div>
  );
}

function Favourites() {
  const [tracks, setTracks] = useState<Track[]>([]);
  useEffect(() => {
    api.GET("/favorites").then(({ data }) => setTracks((data?.tracks ?? []) as Track[]));
  }, []);
  return <TrackList tracks={tracks} listId="favourites" emptyMessage="No favourites yet — tap ♡ on a track." />;
}
