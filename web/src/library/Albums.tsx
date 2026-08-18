import { useState } from "react";
import { imageUrl } from "../jellyfin/client";
import { useAlbums, useAlbumTracks, type Item } from "../jellyfin/queries";
import { useSession } from "../jellyfin/SessionProvider";

export default function Albums() {
  const [selected, setSelected] = useState<Item | null>(null);

  return selected ? (
    <AlbumDetail album={selected} onBack={() => setSelected(null)} />
  ) : (
    <AlbumGrid onSelect={setSelected} />
  );
}

function AlbumGrid({ onSelect }: { onSelect: (album: Item) => void }) {
  const { data: albums, isPending, error } = useAlbums();

  if (isPending) return <p className="muted">Loading…</p>;
  if (error) return <p className="error">Could not load the library.</p>;
  if (!albums.length) return <p className="muted">No albums here yet.</p>;

  return (
    <div className="grid">
      {albums.map((album) => (
        <button
          key={album.Id}
          type="button"
          className="card-button"
          onClick={() => onSelect(album)}
        >
          <Cover item={album} />
          <span>{album.Name}</span>
          <span className="muted">{album.AlbumArtist}</span>
        </button>
      ))}
    </div>
  );
}

function AlbumDetail({ album, onBack }: { album: Item; onBack: () => void }) {
  const { data: tracks, isPending, error } = useAlbumTracks(album.Id ?? null);

  return (
    <section>
      <button type="button" className="link" onClick={onBack}>
        ← Albums
      </button>

      <header className="album-header">
        <Cover item={album} size={300} />
        <div>
          <h2>{album.Name}</h2>
          <p className="muted">{album.AlbumArtist}</p>
        </div>
      </header>

      {isPending && <p className="muted">Loading…</p>}
      {error && <p className="error">Could not load these tracks.</p>}

      <ol className="tracks">
        {tracks?.map((track) => (
          <li key={track.Id}>
            <span className="muted">{track.IndexNumber}</span>
            <span>{track.Name}</span>
          </li>
        ))}
      </ol>
    </section>
  );
}

/**
 * Cover art, or a placeholder when the item has none.
 *
 * Checked via ImageTags rather than by letting the <img> fail: Jellyfin answers
 * a missing image with a 404, and a broken-image icon reads as an error rather
 * than as an album that simply has no artwork.
 */
function Cover({ item, size = 200 }: { item: Item; size?: number }) {
  const { session } = useSession();
  const hasImage = !!item.ImageTags?.["Primary"];

  if (!session || !hasImage || !item.Id) {
    return <div className="placeholder" aria-hidden="true" />;
  }
  return (
    <img
      src={imageUrl(session, item.Id, size)}
      alt=""
      loading="lazy"
      width={size}
      height={size}
    />
  );
}
