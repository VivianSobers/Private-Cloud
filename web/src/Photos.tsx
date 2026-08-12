import { useCallback, useEffect, useRef, useState } from "react";

import { api, ApiError, formatDate, type Album, type Node, type SimilarHit } from "./api";

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

  // Selection mode: pick photos, then add them to an album in one call.
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [adding, setAdding] = useState(false);

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

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  }, []);

  const cancelSelect = useCallback(() => {
    setSelected(new Set());
    setSelecting(false);
  }, []);

  if (error) return <Unavailable what="timeline" detail={error} />;
  if (!items) return <p className="muted">Loading photos…</p>;
  if (items.length === 0)
    return <p className="muted">No photos yet — upload some images and they'll appear here.</p>;

  return (
    <>
      <div className="row small photos-toolbar">
        {selecting ? (
          <>
            <span className="muted">{selected.size} selected</span>
            <button disabled={selected.size === 0} onClick={() => setAdding(true)}>
              Add to album…
            </button>
            <button className="link" onClick={cancelSelect}>
              Cancel
            </button>
          </>
        ) : (
          <button className="link" onClick={() => setSelecting(true)}>
            Select
          </button>
        )}
      </div>

      <PhotoGrid
        nodes={items}
        onOpen={setLightbox}
        selection={selecting ? { selected, onToggle: toggle } : undefined}
      />
      {lightbox && <Lightbox node={lightbox} onClose={() => setLightbox(null)} />}
      {adding && (
        <AddToAlbumDialog
          nodeIds={[...selected]}
          onClose={() => setAdding(false)}
          onDone={() => {
            setAdding(false);
            cancelSelect();
          }}
        />
      )}
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
  const [manage, setManage] = useState(false);
  const [cover, setCover] = useState<string | undefined>(album.cover_node_id);
  const [busy, setBusy] = useState(false);

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

  // Reordering replaces the whole order in one call — the contract's rule, so a
  // drag that issues N updates can't end up half-applied.
  const persistOrder = useCallback(
    async (next: Node[]) => {
      setItems(next);
      try {
        await api.reorderAlbum(album.id, next.map((n) => n.id));
      } catch (e) {
        setError(describe(e));
      }
    },
    [album.id],
  );

  const move = useCallback(
    (index: number, dir: -1 | 1) => {
      setItems((cur) => {
        if (!cur) return cur;
        const j = index + dir;
        if (j < 0 || j >= cur.length) return cur;
        const next = [...cur];
        const a = next[index];
        const b = next[j];
        if (!a || !b) return cur;
        next[index] = b;
        next[j] = a;
        void persistOrder(next);
        return next;
      });
    },
    [persistOrder],
  );

  const remove = useCallback(
    async (n: Node) => {
      setBusy(true);
      try {
        await api.removeFromAlbum(album.id, n.id);
        setItems((cur) => cur?.filter((x) => x.id !== n.id) ?? cur);
      } catch (e) {
        setError(describe(e));
      } finally {
        setBusy(false);
      }
    },
    [album.id],
  );

  const setAsCover = useCallback(
    async (n: Node) => {
      try {
        await api.updateAlbum(album.id, { cover_node_id: n.id });
        setCover(n.id);
      } catch (e) {
        setError(describe(e));
      }
    },
    [album.id],
  );

  return (
    <>
      <div className="row small" style={{ marginBottom: "0.75rem" }}>
        <button className="link" onClick={onBack}>
          ← Albums
        </button>
        <strong>{album.name}</strong>
        {album.description && <span className="muted">{album.description}</span>}
        <span style={{ flex: 1 }} />
        {items && items.length > 0 && (
          <button className="link" onClick={() => setManage((m) => !m)}>
            {manage ? "Done" : "Manage"}
          </button>
        )}
      </div>
      {error && <div className="banner error small">{error}</div>}
      {!items ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <p className="muted">This album is empty. Add photos from the timeline.</p>
      ) : manage ? (
        <div className="photo-grid">
          {items.map((n, i) => (
            <figure key={n.id} className={`managed-tile${cover === n.id ? " is-cover" : ""}`}>
              <Thumb id={n.id} alt={n.name} />
              <figcaption className="tile-actions">
                <button title="Move earlier" disabled={i === 0} onClick={() => move(i, -1)}>
                  ↑
                </button>
                <button title="Move later" disabled={i === items.length - 1} onClick={() => move(i, 1)}>
                  ↓
                </button>
                <button title="Set as cover" onClick={() => void setAsCover(n)}>
                  ★
                </button>
                <button title="Remove from album" disabled={busy} onClick={() => void remove(n)}>
                  ✕
                </button>
              </figcaption>
            </figure>
          ))}
        </div>
      ) : (
        <PhotoGrid nodes={items} onOpen={setLightbox} />
      )}
      {lightbox && <Lightbox node={lightbox} onClose={() => setLightbox(null)} />}
    </>
  );
}

/** A modal that adds the selected nodes to an existing album, or a new one. */
function AddToAlbumDialog({
  nodeIds,
  onClose,
  onDone,
}: {
  nodeIds: string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const [albums, setAlbums] = useState<Album[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    ref.current?.showModal();
    api
      .albums()
      .then((r) => setAlbums(r.albums))
      .catch((e) => setError(describe(e)));
  }, []);

  const addTo = useCallback(
    async (albumId: string) => {
      setBusy(true);
      try {
        await api.addToAlbum(albumId, nodeIds);
        onDone();
      } catch (e) {
        setError(describe(e));
        setBusy(false);
      }
    },
    [nodeIds, onDone],
  );

  const createAndAdd = useCallback(async () => {
    const name = window.prompt("New album name")?.trim();
    if (!name) return;
    setBusy(true);
    try {
      const { album } = await api.createAlbum(name);
      await api.addToAlbum(album.id, nodeIds);
      onDone();
    } catch (e) {
      setError(describe(e));
      setBusy(false);
    }
  }, [nodeIds, onDone]);

  return (
    <dialog ref={ref} onClose={onClose} onCancel={onClose}>
      <div className="stack">
        <div className="row">
          <strong>
            Add {nodeIds.length} {nodeIds.length === 1 ? "photo" : "photos"} to…
          </strong>
          <span style={{ flex: 1 }} />
          <button onClick={onClose}>Close</button>
        </div>
        {error && <div className="banner error small">{error}</div>}
        <button className="primary" disabled={busy} onClick={() => void createAndAdd()}>
          New album…
        </button>
        {albums === null ? (
          <p className="muted small">Loading albums…</p>
        ) : albums.length === 0 ? (
          <p className="muted small">No albums yet — create one above.</p>
        ) : (
          <ul className="album-picker">
            {albums.map((a) => (
              <li key={a.id}>
                <button className="link" disabled={busy} onClick={() => void addTo(a.id)}>
                  {a.name} <span className="muted small">({a.item_count})</span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </dialog>
  );
}

function PhotoGrid({
  nodes,
  onOpen,
  selection,
}: {
  nodes: Node[];
  onOpen: (n: Node) => void;
  selection?: { selected: Set<string>; onToggle: (id: string) => void };
}) {
  return (
    <div className="photo-grid">
      {nodes.map((n) => {
        const sel = selection?.selected.has(n.id) ?? false;
        return (
          <button
            key={n.id}
            className={`photo-tile${sel ? " selected" : ""}`}
            onClick={() => (selection ? selection.onToggle(n.id) : onOpen(n))}
            title={n.name}
          >
            <Thumb id={n.id} alt={n.name} />
            {selection && <span className="tile-check">{sel ? "✓" : ""}</span>}
          </button>
        );
      })}
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
  // The viewer tracks a "current" node so the similar strip can navigate within
  // the overlay without closing it.
  const [current, setCurrent] = useState<Node>(node);
  const [similar, setSimilar] = useState<SimilarHit[] | null>(null);
  const [showSimilar, setShowSimilar] = useState(false);

  useEffect(() => {
    setCurrent(node);
  }, [node]);

  // Escape closes it, like every other overlay in the app.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const [src, setSrc] = useState(api.contentUrl(current.id, "preview"));
  useEffect(() => {
    setSrc(api.contentUrl(current.id, "preview"));
    setShowSimilar(false);
    setSimilar(null);
  }, [current.id]);

  const findSimilar = async () => {
    setShowSimilar(true);
    try {
      const r = await api.similar(current.id);
      setSimilar(r.results.filter((n) => n.id !== current.id));
    } catch {
      setSimilar([]);
    }
  };

  return (
    <div className="lightbox" onClick={onClose} role="dialog" aria-label={current.name}>
      <img
        className="lightbox-img"
        src={src}
        alt={current.name}
        onClick={(e) => e.stopPropagation()}
        // If the preview rendition isn't ready, show the original rather than nothing.
        onError={() => setSrc(api.contentUrl(current.id, "original"))}
      />
      <div className="lightbox-caption" onClick={(e) => e.stopPropagation()}>
        <span>{current.name}</span>
        <span className="muted small">{formatDate(takenAt(current))}</span>
        <button className="link" onClick={() => void findSimilar()}>
          Find similar
        </button>
        <a className="link" href={api.downloadUrl(current.id, true)}>
          Download
        </a>
      </div>

      {showSimilar && (
        <div className="sim-strip" onClick={(e) => e.stopPropagation()}>
          {similar === null ? (
            <span className="muted small">Finding similar…</span>
          ) : similar.length === 0 ? (
            <span className="muted small">No similar files found.</span>
          ) : (
            similar.map((n) => (
              <button
                key={n.id}
                className="sim-thumb"
                title={n.name}
                onClick={() => setCurrent(n)}
              >
                <Thumb id={n.id} alt={n.name} />
              </button>
            ))
          )}
        </div>
      )}
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
