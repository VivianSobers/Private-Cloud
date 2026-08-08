import { useCallback, useEffect, useState } from "react";

import { api, ApiError, formatDate, type Album, type Node } from "./api";

// The Photos view: a timeline of media sorted by when the shutter fired, and
// hand-ordered albums. It reads the Phase 5 media surface (see the API contract);
// where the server has not implemented an endpoint yet, each panel degrades to a
// clear empty/unavailable state rather than a broken screen.

type Tab = "timeline" | "albums";

export function Photos() {
  const [tab, setTab] = useState<Tab>("timeline");
  const [openAlbum, setOpenAlbum] = useState<Album | null>(null);

  return (
    <section className="photos">
      <nav className="row small photos-tabs">
        <button
          className="link"
          aria-current={tab === "timeline" && !openAlbum}
          onClick={() => {
            setTab("timeline");
            setOpenAlbum(null);
          }}
        >
          Timeline
        </button>
        <button
          className="link"
          aria-current={tab === "albums" || !!openAlbum}
          onClick={() => {
            setTab("albums");
            setOpenAlbum(null);
          }}
        >
          Albums
        </button>
      </nav>

      {openAlbum ? (
        <AlbumDetail album={openAlbum} onBack={() => setOpenAlbum(null)} />
      ) : tab === "timeline" ? (
        <Timeline />
      ) : (
        <Albums onOpen={setOpenAlbum} />
      )}
    </section>
  );
}

/** takenAt is the media's shutter time, falling back to the node's own update
 *  time — the fallback the contract says the client must make visible itself. */
function takenAt(n: Node): string {
  return n.media?.taken_at ?? n.updated_at;
}

function Timeline() {
  const [items, setItems] = useState<Node[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<Node | null>(null);

  useEffect(() => {
    let live = true;
    api
      .timeline({ limit: 200 })
      .then((r) => live && setItems(r.items))
      .catch((e) => live && setError(describe(e)));
    return () => {
      live = false;
    };
  }, []);

  if (error) return <Unavailable what="timeline" detail={error} />;
  if (!items) return <p className="muted">Loading photos…</p>;
  if (items.length === 0)
    return <p className="muted">No photos yet — upload some images and they'll appear here.</p>;

  return (
    <>
      <PhotoGrid nodes={items} onOpen={setLightbox} />
      {lightbox && <Lightbox node={lightbox} onClose={() => setLightbox(null)} />}
    </>
  );
}

function Albums({ onOpen }: { onOpen: (a: Album) => void }) {
  const [albums, setAlbums] = useState<Album[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .albums()
      .then((r) => setAlbums(r.albums))
      .catch((e) => setError(describe(e)));
  }, []);

  useEffect(load, [load]);

  const create = useCallback(async () => {
    const name = window.prompt("Album name")?.trim();
    if (!name) return;
    setBusy(true);
    try {
      await api.createAlbum(name);
      setError(null);
      load();
    } catch (e) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  }, [load]);

  return (
    <>
      <div className="row small" style={{ marginBottom: "0.75rem" }}>
        <button onClick={create} disabled={busy}>
          New album
        </button>
      </div>
      {error ? (
        <Unavailable what="albums" detail={error} />
      ) : !albums ? (
        <p className="muted">Loading albums…</p>
      ) : albums.length === 0 ? (
        <p className="muted">No albums yet. Create one to group photos without moving the files.</p>
      ) : (
        <div className="album-grid">
          {albums.map((a) => (
            <button key={a.id} className="album-tile" onClick={() => onOpen(a)} title={a.name}>
              <div className="album-cover">
                {a.cover_node_id ? (
                  <Thumb id={a.cover_node_id} alt={a.name} />
                ) : (
                  <span className="album-cover-empty" aria-hidden="true" />
                )}
              </div>
              <div className="album-meta">
                <span className="album-name">{a.name}</span>
                <span className="muted small">
                  {a.item_count} {a.item_count === 1 ? "item" : "items"}
                </span>
              </div>
            </button>
          ))}
        </div>
      )}
    </>
  );
}

function AlbumDetail({ album, onBack }: { album: Album; onBack: () => void }) {
  const [items, setItems] = useState<Node[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<Node | null>(null);

  useEffect(() => {
    let live = true;
    api
      .album(album.id, { limit: 500 })
      .then((r) => live && setItems(r.items))
      .catch((e) => live && setError(describe(e)));
    return () => {
      live = false;
    };
  }, [album.id]);

  return (
    <>
      <div className="row small" style={{ marginBottom: "0.75rem" }}>
        <button className="link" onClick={onBack}>
          ← Albums
        </button>
        <strong>{album.name}</strong>
        {album.description && <span className="muted">{album.description}</span>}
      </div>
      {error ? (
        <Unavailable what="album" detail={error} />
      ) : !items ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <p className="muted">This album is empty.</p>
      ) : (
        <PhotoGrid nodes={items} onOpen={setLightbox} />
      )}
      {lightbox && <Lightbox node={lightbox} onClose={() => setLightbox(null)} />}
    </>
  );
}

function PhotoGrid({ nodes, onOpen }: { nodes: Node[]; onOpen: (n: Node) => void }) {
  return (
    <div className="photo-grid">
      {nodes.map((n) => (
        <button key={n.id} className="photo-tile" onClick={() => onOpen(n)} title={n.name}>
          <Thumb id={n.id} alt={n.name} />
        </button>
      ))}
    </div>
  );
}

/** A thumbnail that falls back to a labelled placeholder when the `thumb` variant
 *  is not ready yet, rather than pulling the full-size original for every tile. */
function Thumb({ id, alt }: { id: string; alt: string }) {
  const [broken, setBroken] = useState(false);
  if (broken) return <span className="thumb-fallback">{alt}</span>;
  return (
    <img
      className="thumb"
      src={api.contentUrl(id, "thumb")}
      alt={alt}
      loading="lazy"
      onError={() => setBroken(true)}
    />
  );
}

function Lightbox({ node, onClose }: { node: Node; onClose: () => void }) {
  // Escape closes it, like every other overlay in the app.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const [src, setSrc] = useState(api.contentUrl(node.id, "preview"));
  return (
    <div className="lightbox" onClick={onClose} role="dialog" aria-label={node.name}>
      <img
        className="lightbox-img"
        src={src}
        alt={node.name}
        onClick={(e) => e.stopPropagation()}
        // If the preview rendition isn't ready, show the original rather than nothing.
        onError={() => setSrc(api.contentUrl(node.id, "original"))}
      />
      <div className="lightbox-caption" onClick={(e) => e.stopPropagation()}>
        <span>{node.name}</span>
        <span className="muted small">{formatDate(takenAt(node))}</span>
        <a className="link" href={api.downloadUrl(node.id, true)}>
          Download
        </a>
      </div>
    </div>
  );
}

function Unavailable({ what, detail }: { what: string; detail: string }) {
  return (
    <div className="muted">
      <p>Photos {what} isn't available yet.</p>
      <p className="small">{detail}</p>
    </div>
  );
}

/** describe turns an API failure into a short line. A 404 here usually means the
 *  media endpoints aren't deployed yet, so it is worded as such. */
function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 404) return "The media endpoints are not available on this server yet.";
    return `${e.code}: ${e.message}${e.requestId ? ` (${e.requestId})` : ""}`;
  }
  return e instanceof Error ? e.message : "Unknown error";
}
