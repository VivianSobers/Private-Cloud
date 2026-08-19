import { useCallback, useEffect, useRef, useState, type PointerEvent } from "react";

import {
  api,
  ApiError,
  formatDate,
  type Album,
  type Face,
  type Node,
  type Person,
  type SimilarHit,
} from "./api";
import {
  clusterPlaced,
  fitView,
  formatLat,
  formatLon,
  gridStep,
  normaliseLon,
  project,
  unprojectLat,
  type Cluster,
  type Placed,
} from "./geo";
import { isPinned, pinFile, supportsPinning, unpinFile } from "./pin";

// The Photos view: a timeline of media sorted by when the shutter fired, and
// hand-ordered albums. It reads the Phase 5 media surface (see the API contract);
// where the server has not implemented an endpoint yet, each panel degrades to a
// clear empty/unavailable state rather than a broken screen.

type Tab = "timeline" | "map" | "albums";

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
          aria-current={tab === "map" && !openAlbum}
          onClick={() => {
            setTab("map");
            setOpenAlbum(null);
          }}
        >
          Map
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
      ) : tab === "map" ? (
        <PhotoMap />
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

// --- the map ---------------------------------------------------------------
//
// The server has served `gps` on every geotagged photo, exact and unrounded,
// since Phase 5 — nothing rendered it. What held this up was not the drawing:
// it was that a map usually means a tile provider, and an offline-capable app
// whose users hold their own data cannot quietly send the coordinates of every
// photo they own to a stranger's server, one HTTP request per tile, every time
// somebody pans.
//
// So this map has no tiles and no map library. It draws a graticule and plots
// the photos on it, projected the way every web map projects, and it works with
// the network off. That is less pretty than a street map and it is the whole
// point: nothing here phones anywhere. A single "open in OpenStreetMap" link per
// place is offered inside the strip, because one deliberate click that the user
// makes is a different thing from a hundred the page makes for them.
//
// Web Mercator, so shapes and bearings look the way people expect, and clamped
// at the poles where the projection runs to infinity.
function PhotoMap() {
  const [items, setItems] = useState<Node[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [lightbox, setLightbox] = useState<Node | null>(null);
  const [selected, setSelected] = useState<Cluster | null>(null);

  // The view: a centre in projected units and a half-span. Fitted to the photos
  // once they arrive, then moved by the user.
  const [view, setView] = useState({ cx: 0, cy: 0, span: 180 });
  const svgRef = useRef<SVGSVGElement>(null);
  const dragging = useRef<{ x: number; y: number } | null>(null);

  useEffect(() => {
    let live = true;
    // The timeline is the only listing that carries media metadata, so it is
    // also the source for the map. Paged rather than asked for everything at
    // once: a library can be large, and a map that has to load all of it before
    // drawing anything shows nothing for a long time.
    void (async () => {
      const collected: Node[] = [];
      let offset = 0;
      let more = true;
      try {
        for (let page = 0; page < 10 && more; page++) {
          const res = await api.timeline({ limit: 200, offset });
          if (!live) return;
          collected.push(...res.items.filter((n) => n.media?.gps));
          more = res.has_more;
          offset += res.items.length;
          if (res.items.length === 0) break;
        }
        if (!live) return;
        setTruncated(more);
        setItems(collected);
      } catch (e) {
        if (live) setError(describe(e));
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  const placed: Placed[] = (items ?? []).flatMap((node) => {
    const gps = node.media?.gps;
    if (!gps) return [];
    return [{ node, lat: gps.lat, lon: gps.lon, ...project(gps.lat, gps.lon) }];
  });

  // Fit the view to the photos the first time they land, and never again: after
  // that the view is the user's, and refitting would undo their pan.
  const fitted = useRef(false);
  useEffect(() => {
    if (fitted.current || placed.length === 0) return;
    fitted.current = true;
    setView(fitView(placed));
  }, [placed]);

  const zoom = useCallback((factor: number) => {
    setView((v) => ({ ...v, span: Math.min(180, Math.max(0.002, v.span * factor)) }));
  }, []);

  // Panning converts pixels to view units through the rendered width, so a drag
  // moves the map by the distance under the pointer at any zoom.
  const onPointerDown = (e: PointerEvent<SVGSVGElement>) => {
    dragging.current = { x: e.clientX, y: e.clientY };
    e.currentTarget.setPointerCapture(e.pointerId);
  };

  const onPointerMove = (e: PointerEvent<SVGSVGElement>) => {
    const from = dragging.current;
    const box = svgRef.current?.getBoundingClientRect();
    if (!from || !box || box.width === 0) return;
    const unitsPerPixel = (view.span * 2) / box.width;
    setView((v) => ({
      ...v,
      cx: v.cx - (e.clientX - from.x) * unitsPerPixel,
      cy: v.cy - (e.clientY - from.y) * unitsPerPixel,
    }));
    dragging.current = { x: e.clientX, y: e.clientY };
  };

  const onPointerUp = () => {
    dragging.current = null;
  };

  if (error) return <Unavailable what="the map" detail={error} />;
  if (!items) return <p className="muted">Loading photos…</p>;
  if (placed.length === 0)
    return (
      <p className="muted">
        No photos with a location yet. Coordinates come from the EXIF a camera
        writes, so a photo that was taken with location off, or stripped by
        another app before upload, has none to show.
      </p>
    );

  const step = gridStep(view.span * 2);
  const left = view.cx - view.span;
  const right = view.cx + view.span;
  const top = view.cy - view.span;
  const bottom = view.cy + view.span;

  const verticals: number[] = [];
  for (let x = Math.ceil(left / step) * step; x <= right; x += step) verticals.push(x);
  const horizontals: number[] = [];
  for (let y = Math.ceil(top / step) * step; y <= bottom; y += step) horizontals.push(y);

  // Cluster at roughly 26 screen pixels, expressed in view units so the grouping
  // loosens as you zoom out and separates as you zoom in.
  const clusters = clusterPlaced(placed, (view.span * 2 * 26) / 600);

  return (
    <>
      <div className="row small photos-toolbar">
        <span className="muted">
          {placed.length} photo{placed.length === 1 ? "" : "s"} with a location
          {truncated && " (most recent 2000)"}
        </span>
        <span style={{ flex: 1 }} />
        <button className="link" onClick={() => zoom(0.6)} title="Zoom in">
          +
        </button>
        <button className="link" onClick={() => zoom(1.6)} title="Zoom out">
          −
        </button>
        <button
          className="link"
          onClick={() => {
            fitted.current = false;
            setView({ cx: 0, cy: 0, span: 180 });
            setSelected(null);
          }}
          title="Show everything again"
        >
          Reset
        </button>
      </div>

      <svg
        ref={svgRef}
        className="photo-map"
        viewBox={`${left} ${top} ${view.span * 2} ${view.span * 2}`}
        preserveAspectRatio="xMidYMid meet"
        role="img"
        aria-label={`Map of ${placed.length} geotagged photos`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
      >
        {verticals.map((x) => (
          <g key={`v${x}`}>
            <line x1={x} y1={top} x2={x} y2={bottom} className="graticule" />
            <text x={x} y={top + view.span * 0.06} className="graticule-label">
              {formatLon(normaliseLon(x))}
            </text>
          </g>
        ))}
        {horizontals.map((y) => (
          <g key={`h${y}`}>
            <line x1={left} y1={y} x2={right} y2={y} className="graticule" />
            <text x={left + view.span * 0.02} y={y - view.span * 0.01} className="graticule-label">
              {formatLat(unprojectLat(y))}
            </text>
          </g>
        ))}

        {clusters.map((c) => (
          <g
            key={`${c.x},${c.y}`}
            className={`map-pin${selected === c ? " is-selected" : ""}`}
            onClick={() => setSelected(c)}
          >
            <circle cx={c.x} cy={c.y} r={view.span * 0.035} />
            {c.items.length > 1 && (
              <text x={c.x} y={c.y + view.span * 0.014} className="map-pin-count">
                {c.items.length}
              </text>
            )}
          </g>
        ))}
      </svg>

      <p className="muted small">
        Drag to pan. No map tiles are fetched from anywhere — this is drawn from
        your photos' own coordinates, so it works with the network off.
      </p>

      {selected && (
        <section className="stack">
          <div className="row small">
            <strong>
              {formatLat(selected.lat)} {formatLon(selected.lon)}
            </strong>
            <span className="muted">
              {selected.items.length} photo{selected.items.length === 1 ? "" : "s"}
            </span>
            <span style={{ flex: 1 }} />
            {/* One deliberate click, not a request per tile. */}
            <a
              className="link"
              href={`https://www.openstreetmap.org/?mlat=${selected.lat}&mlon=${selected.lon}#map=14/${selected.lat}/${selected.lon}`}
              target="_blank"
              rel="noreferrer noopener"
            >
              Open in OpenStreetMap ↗
            </a>
            <button className="link" onClick={() => setSelected(null)}>
              Close
            </button>
          </div>
          <PhotoGrid nodes={selected.items.map((i) => i.node)} onOpen={setLightbox} />
        </section>
      )}

      {lightbox && <Lightbox node={lightbox} onClose={() => setLightbox(null)} />}
    </>
  );
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

  // Pointer-drag reordering. The tile being carried is tracked by index, and
  // passing over another tile reorders the local list immediately, so the grid
  // shows the result while the pointer is still down rather than jumping when it
  // is released.
  //
  // The server is told once, on drop — the contract's rule is that an order is
  // replaced wholesale rather than patched per item, and dragging across ten
  // tiles would otherwise be ten chances to end up half-applied. The move-up and
  // move-down buttons stay: they are the keyboard and touch path, and a grid
  // that can only be reordered by dragging cannot be reordered by everybody.
  const dragFrom = useRef<number | null>(null);

  const dragOver = useCallback((index: number) => {
    setItems((cur) => {
      const from = dragFrom.current;
      if (!cur || from === null || from === index) return cur;
      const next = [...cur];
      const [carried] = next.splice(from, 1);
      if (!carried) return cur;
      next.splice(index, 0, carried);
      dragFrom.current = index;
      return next;
    });
  }, []);

  // The order to persist is read from a ref rather than from inside a state
  // updater: an updater has to be pure, and firing the request from inside one
  // would send it twice under React's development double-invoke.
  const latest = useRef<Node[] | null>(null);
  latest.current = items;

  const dropped = useCallback(() => {
    if (dragFrom.current === null) return;
    dragFrom.current = null;
    if (latest.current) void persistOrder(latest.current);
  }, [persistOrder]);

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
            <figure
              key={n.id}
              className={`managed-tile${cover === n.id ? " is-cover" : ""}`}
              draggable
              onDragStart={(e) => {
                dragFrom.current = i;
                e.dataTransfer.effectAllowed = "move";
                // Firefox starts no drag at all without payload on the transfer.
                e.dataTransfer.setData("text/plain", n.id);
              }}
              onDragOver={(e) => {
                // Without preventDefault the element is not a drop target and
                // the browser shows the "no drop" cursor over every tile.
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                dragOver(i);
              }}
              onDrop={(e) => {
                e.preventDefault();
                dropped();
              }}
              // A drag released outside the grid still ends here, so the order on
              // screen is saved rather than silently reverting on the next load.
              onDragEnd={dropped}
            >
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
  const [faces, setFaces] = useState<Face[] | null>(null);
  const [showFaces, setShowFaces] = useState(false);

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
  const [pinned, setPinned] = useState(isPinned(current.id));
  useEffect(() => {
    setSrc(api.contentUrl(current.id, "preview"));
    setShowSimilar(false);
    setSimilar(null);
    setShowFaces(false);
    setFaces(null);
    setPinned(isPinned(current.id));
  }, [current.id]);

  const findFaces = async () => {
    setShowFaces(true);
    try {
      const r = await api.nodeFaces(current.id);
      setFaces(r.faces);
    } catch {
      setFaces([]);
    }
  };

  // After a reassign, re-fetch so the panel reflects the new assignment rather
  // than optimistically guessing what the server did.
  const reloadFaces = async () => {
    try {
      const r = await api.nodeFaces(current.id);
      setFaces(r.faces);
    } catch {
      // Leave the current list; the row's own state already changed.
    }
  };

  const togglePin = async () => {
    try {
      if (pinned) {
        await unpinFile(current.id);
        setPinned(false);
      } else {
        await pinFile({ id: current.id, name: current.name, path: current.path, size: current.size });
        setPinned(true);
      }
    } catch {
      // A pin can fail offline or unauthorized; leave the state as it was.
    }
  };

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
      <div className="lightbox-stage" onClick={(e) => e.stopPropagation()}>
        <img
          className="lightbox-img"
          src={src}
          alt={current.name}
          // If the preview rendition isn't ready, show the original rather than nothing.
          onError={() => setSrc(api.contentUrl(current.id, "original"))}
        />
        {/* Boxes over the faces, positioned as fractions of the frame. Drawn only
            while the panel is open so the photo is unobstructed by default. */}
        {showFaces &&
          faces?.map((f) => (
            <span
              key={f.id}
              className={`face-box${f.person_id ? "" : " unassigned"}`}
              style={{
                left: `${f.box[0] * 100}%`,
                top: `${f.box[1] * 100}%`,
                width: `${f.box[2] * 100}%`,
                height: `${f.box[3] * 100}%`,
              }}
            />
          ))}
      </div>
      <div className="lightbox-caption" onClick={(e) => e.stopPropagation()}>
        <span>{current.name}</span>
        <span className="muted small">{formatDate(takenAt(current))}</span>
        <button className="link" onClick={() => void findSimilar()}>
          Find similar
        </button>
        <button className="link" onClick={() => void findFaces()}>
          Who's here
        </button>
        {supportsPinning() && (
          <button className="link" onClick={() => void togglePin()}>
            {pinned ? "Pinned offline ✓" : "Pin offline"}
          </button>
        )}
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

      {showFaces && (
        <div className="faces-panel" onClick={(e) => e.stopPropagation()}>
          <FacesPanel nodeId={current.id} faces={faces} onChanged={() => void reloadFaces()} />
        </div>
      )}
    </div>
  );
}

/** FacesPanel lists the faces detected in a photo and lets a user correct a
 *  wrong one: point it at the right person, or detach it entirely. Naming a
 *  cluster lives in the People view; here we only reassign existing people, so
 *  the panel needs no text entry — just the roster the server already knows. */
function FacesPanel({
  nodeId,
  faces,
  onChanged,
}: {
  nodeId: string;
  faces: Face[] | null;
  onChanged: () => void;
}) {
  const [people, setPeople] = useState<Person[] | null>(null);

  useEffect(() => {
    api
      .people()
      .then((r) => setPeople(r.people))
      .catch(() => setPeople([]));
  }, []);

  const nameFor = (id?: string): string => {
    if (!id) return "Unassigned";
    const p = people?.find((x) => x.id === id);
    if (!p) return "Someone";
    return p.name ?? "Unnamed person";
  };

  const reassign = async (faceId: string, personId: string | null) => {
    try {
      await api.reassignFace(nodeId, faceId, personId);
      onChanged();
    } catch {
      // Leave the roster as-is; the change simply didn't take.
    }
  };

  if (faces === null) return <span className="muted small">Looking for faces…</span>;
  if (faces.length === 0) return <span className="muted small">No faces detected here.</span>;

  return (
    <ul className="faces-list">
      {faces.map((f) => (
        <li key={f.id} className="face-row">
          <span className={f.person_id ? "" : "muted"}>{nameFor(f.person_id)}</span>
          <span className="face-actions">
            <select
              value={f.person_id ?? ""}
              onChange={(e) => void reassign(f.id, e.target.value || null)}
              aria-label="Assign this face to a person"
            >
              <option value="">— unassigned —</option>
              {people?.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name ?? "Unnamed person"}
                </option>
              ))}
            </select>
            {f.person_id && (
              <button className="link small" onClick={() => void reassign(f.id, null)}>
                Not a face
              </button>
            )}
          </span>
        </li>
      ))}
    </ul>
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
