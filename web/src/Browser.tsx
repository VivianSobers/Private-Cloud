import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api, formatBytes, formatDate, type Node, type SearchHit, type Usage } from "./api";
import { ShareDialog } from "./ShareDialog";
import { Shares } from "./Shares";
import { Trash } from "./Trash";
import { Tags } from "./Tags";
import { Versions } from "./Versions";
import { upload, type UploadHandle } from "./upload";

/** Below this, the server refuses the query rather than scan the whole tree. */
const MIN_QUERY = 2;

interface Transfer {
  id: number;
  name: string;
  sent: number;
  total: number;
  error?: string;
  handle: UploadHandle;
}

let transferSeq = 0;

export function Browser() {
  const [folder, setFolder] = useState<Node | null>(null);
  const [children, setChildren] = useState<Node[]>([]);
  const [usage, setUsage] = useState<Usage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [transfers, setTransfers] = useState<Transfer[]>([]);
  const [dragOver, setDragOver] = useState(false);
  const [showTrash, setShowTrash] = useState(false);
  const [showShares, setShowShares] = useState(false);
  // The file whose version history is open, or null. A file, never a folder.
  const [versionsFor, setVersionsFor] = useState<Node | null>(null);
  // The node being shared, or null. Files and folders both.
  const [shareFor, setShareFor] = useState<Node | null>(null);
  const [tagsFor, setTagsFor] = useState<Node | null>(null);

  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<SearchHit[] | null>(null);
  const [searchScoped, setSearchScoped] = useState(false);
  const [semantic, setSemantic] = useState(false);

  const fileInput = useRef<HTMLInputElement>(null);

  const load = useCallback(async (id?: string) => {
    setLoading(true);
    setError(null);
    try {
      const target = id ?? (await api.root()).node.id;
      const res = await api.children(target);
      setFolder(res.parent);
      setChildren(res.children);
      setUsage(await api.usage());
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Debounced search. Without the delay every keystroke is a trigram scan, and
  // responses can arrive out of order — the `cancelled` flag is what stops a
  // slow early query from overwriting the results of a later, faster one.
  useEffect(() => {
    const text = query.trim();
    if (text.length < MIN_QUERY) {
      setHits(null);
      return;
    }
    let cancelled = false;
    const under = searchScoped ? folder?.path : undefined;
    const timer = setTimeout(() => {
      void api
        .search(text, { under, semantic })
        .then((res) => {
          if (!cancelled) setHits(res.results);
        })
        .catch((err) => {
          // Semantic search is optional; if no sidecar is configured the server
          // answers 503. Fall back to lexical search rather than showing an error.
          if (semantic && err instanceof ApiError && err.status === 503) {
            return api.search(text, { under }).then((res) => {
              if (!cancelled) setHits(res.results);
            });
          }
          if (!cancelled) setError(err instanceof ApiError ? err.message : String(err));
        });
    }, 200);

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, searchScoped, folder?.path, semantic]);

  const reload = useCallback(() => {
    if (folder) void load(folder.id);
  }, [folder, load]);

  function startUploads(files: FileList | File[]) {
    if (!folder) return;
    const parentId = folder.id;

    for (const file of Array.from(files)) {
      const id = ++transferSeq;
      const patch = (fn: (t: Transfer) => Transfer) =>
        setTransfers((ts) => ts.map((t) => (t.id === id ? fn(t) : t)));

      const handle = upload(file, parentId, {
        onProgress: (sent, total) => patch((t) => ({ ...t, sent, total })),
        onDone: () => {
          setTransfers((ts) => ts.filter((t) => t.id !== id));
          // Reload against the folder the upload targeted, not whatever is on
          // screen now — the user may have navigated away mid-transfer.
          if (parentId === folder.id) void load(parentId);
        },
        onError: (message) => patch((t) => ({ ...t, error: message })),
      });

      setTransfers((ts) => [...ts, { id, name: file.name, sent: 0, total: file.size, handle }]);
    }
  }

  async function guard(fn: () => Promise<unknown>) {
    setError(null);
    try {
      await fn();
      reload();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  if (showTrash) {
    return (
      <Trash
        onClose={() => {
          setShowTrash(false);
          reload();
        }}
      />
    );
  }

  if (showShares) {
    return <Shares onClose={() => setShowShares(false)} />;
  }

  return (
    <div className="stack">
      {versionsFor && (
        <Versions
          node={versionsFor}
          onClose={() => setVersionsFor(null)}
          onRestored={() => void load(folder?.id)}
        />
      )}
      {shareFor && <ShareDialog node={shareFor} onClose={() => setShareFor(null)} />}
      {tagsFor && <Tags node={tagsFor} onClose={() => setTagsFor(null)} />}
      <div className="row">
        <Breadcrumbs folder={folder} onNavigate={(id) => void load(id)} />
        <span style={{ flex: 1 }} />
        {usage && <QuotaBar usage={usage} />}
      </div>

      {error && <div className="banner error">{error}</div>}

      <div className="row">
        <button className="primary" onClick={() => fileInput.current?.click()} disabled={!folder}>
          Upload
        </button>
        <input
          ref={fileInput}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            if (e.target.files) startUploads(e.target.files);
            // Reset so re-selecting the same file fires change again.
            e.target.value = "";
          }}
        />
        <button
          disabled={!folder}
          onClick={() =>
            void guard(async () => {
              const name = window.prompt("New folder name");
              if (!name) return;
              await api.createFolder(folder!.id, name);
            })
          }
        >
          New folder
        </button>
        <button onClick={() => setShowTrash(true)}>Trash</button>
        <button onClick={() => setShowShares(true)}>Shares</button>

        <span style={{ flex: 1 }} />

        <input
          type="search"
          value={query}
          placeholder="Search files…"
          aria-label="Search files"
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") setQuery("");
          }}
        />
        {folder && folder.path !== "/" && (
          <label className="row small muted">
            <input
              type="checkbox"
              checked={searchScoped}
              onChange={(e) => setSearchScoped(e.target.checked)}
            />
            in this folder
          </label>
        )}
        <label className="row small muted" title="Find files by meaning, not just by a word they contain">
          <input type="checkbox" checked={semantic} onChange={(e) => setSemantic(e.target.checked)} />
          by meaning
        </label>
      </div>

      <div
        className={dragOver ? "dropzone over" : "dropzone"}
        onDragOver={(e) => {
          e.preventDefault();
          setDragOver(true);
        }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragOver(false);
          if (e.dataTransfer.files.length) startUploads(e.dataTransfer.files);
        }}
      >
        Drop files here to upload to <strong>{folder?.path ?? "/"}</strong>
      </div>

      {transfers.length > 0 && (
        <div className="card stack">
          {transfers.map((t) => (
            <div key={t.id} className="stack" style={{ gap: "0.3rem" }}>
              <div className="row small">
                <span style={{ flex: 1, overflowWrap: "anywhere" }}>{t.name}</span>
                {t.error ? (
                  <span className="muted" style={{ color: "var(--danger)" }}>
                    {t.error}
                  </span>
                ) : (
                  <span className="muted">
                    {formatBytes(t.sent)} / {formatBytes(t.total)}
                  </span>
                )}
                <button
                  className="link"
                  onClick={() => {
                    t.handle.abort();
                    setTransfers((ts) => ts.filter((x) => x.id !== t.id));
                  }}
                >
                  {t.error ? "Dismiss" : "Cancel"}
                </button>
              </div>
              {!t.error && (
                <div className="progress">
                  <div style={{ width: `${t.total ? (t.sent / t.total) * 100 : 0}%` }} />
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {hits !== null ? (
        <SearchResults
          hits={hits}
          query={query.trim()}
          onOpenFolder={(id) => {
            setQuery("");
            void load(id);
          }}
        />
      ) : loading ? (
        <p className="muted">Loading…</p>
      ) : children.length === 0 ? (
        <div className="empty">This folder is empty.</div>
      ) : (
        <table className="listing">
          <thead>
            <tr>
              <th>Name</th>
              <th style={{ textAlign: "right" }}>Size</th>
              <th className="when" style={{ textAlign: "right" }}>
                Modified
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {children.map((n) => (
              <Row
                key={n.id}
                node={n}
                onOpen={() => void load(n.id)}
                onRename={() =>
                  void guard(async () => {
                    const name = window.prompt("Rename to", n.name);
                    if (!name || name === n.name) return;
                    await api.patchNode(n.id, { name });
                  })
                }
                onMove={() =>
                  void guard(async () => {
                    const dest = window.prompt(
                      "Move to which folder? (absolute path, e.g. /Documents)",
                      folder?.path ?? "/",
                    );
                    if (!dest) return;
                    const target = await api.resolve(dest);
                    if (target.node.kind !== "folder") throw new Error("that path is not a folder");
                    await api.patchNode(n.id, { parent_id: target.node.id });
                  })
                }
                onTrash={() =>
                  void guard(async () => {
                    const what = n.kind === "folder" ? "this folder and everything in it" : "this file";
                    if (!window.confirm(`Move ${what} to the trash?`)) return;
                    await api.trashNode(n.id);
                  })
                }
                onVersions={() => setVersionsFor(n)}
                onShare={() => setShareFor(n)}
                onTags={() => setTagsFor(n)}
              />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function SearchResults({
  hits,
  query,
  onOpenFolder,
}: {
  hits: SearchHit[];
  query: string;
  onOpenFolder: (id: string) => void;
}) {
  if (hits.length === 0) {
    return <div className="empty">Nothing matches “{query}”.</div>;
  }

  return (
    <>
      <p className="muted small">
        {hits.length} result{hits.length === 1 ? "" : "s"} for “{query}”
      </p>
      <table className="listing">
        <thead>
          <tr>
            <th>Name</th>
            <th style={{ textAlign: "right" }}>Size</th>
            <th className="when" style={{ textAlign: "right" }}>
              Modified
            </th>
          </tr>
        </thead>
        <tbody>
          {hits.map((h) => (
            <tr key={h.id}>
              <td className="name">
                {h.kind === "folder" ? (
                  <button onClick={() => onOpenFolder(h.id)}>📁 {h.name}</button>
                ) : (
                  <a href={api.downloadUrl(h.id)} target="_blank" rel="noreferrer">
                    📄 {h.name}
                  </a>
                )}
                {/* The full path, because a result list without it is a pile of
                    identically named files with no way to tell them apart. */}
                <div className="muted small">
                  {h.path}
                  {/* Why this matched, so a filename with no visible relation to
                      the query does not look like a bug. */}
                  {h.matched_path && " · matched the folder name"}
                  {h.matched_content && " · matched the file’s text"}
                  {h.semantic &&
                    typeof h.score === "number" &&
                    ` · semantic match (${Math.round(h.score * 100)}%)`}
                </div>
              </td>
              <td className="size">{h.kind === "file" ? formatBytes(h.size ?? 0) : "—"}</td>
              <td className="when">{formatDate(h.updated_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

function Row({
  node,
  onOpen,
  onRename,
  onMove,
  onTrash,
  onVersions,
  onShare,
  onTags,
}: {
  node: Node;
  onOpen: () => void;
  onRename: () => void;
  onMove: () => void;
  onTrash: () => void;
  onVersions: () => void;
  onShare: () => void;
  onTags: () => void;
}) {
  return (
    <tr>
      <td className="name">
        {node.kind === "folder" ? (
          <button onClick={onOpen}>📁 {node.name}</button>
        ) : (
          // A plain link, not a fetch: the browser gets a real progress
          // indicator, resumable downloads, and can stream video into a
          // player. A blob held in memory does none of that.
          <a href={api.downloadUrl(node.id)} target="_blank" rel="noreferrer">
            📄 {node.name}
          </a>
        )}
      </td>
      <td className="size">{node.kind === "file" ? formatBytes(node.size ?? 0) : "—"}</td>
      <td className="when">{formatDate(node.updated_at)}</td>
      <td className="actions">
        {node.kind === "file" && (
          <a className="small" href={api.downloadUrl(node.id, true)} download>
            Download
          </a>
        )}{" "}
        {node.kind === "file" && (
          <button className="link" onClick={onVersions}>
            History
          </button>
        )}
        <button className="link" onClick={onShare}>
          Share
        </button>
        {node.kind === "file" && (
          <button className="link" onClick={onTags}>
            Tags
          </button>
        )}
        <button className="link" onClick={onRename}>
          Rename
        </button>
        <button className="link" onClick={onMove}>
          Move
        </button>
        <button className="link danger" onClick={onTrash}>
          Delete
        </button>
      </td>
    </tr>
  );
}

function Breadcrumbs({ folder, onNavigate }: { folder: Node | null; onNavigate: (id?: string) => void }) {
  const [ancestors, setAncestors] = useState<Node[]>([]);

  useEffect(() => {
    if (!folder) {
      setAncestors([]);
      return;
    }
    // Walk up by resolving each path prefix. The materialised path makes this
    // a handful of point lookups rather than a recursive query, and the depth
    // is bounded by how deep the user actually is.
    let cancelled = false;
    const segments = folder.path.split("/").filter(Boolean);
    const prefixes = segments.slice(0, -1).map((_, i) => "/" + segments.slice(0, i + 1).join("/"));

    void Promise.all(prefixes.map((p) => api.resolve(p).then((r) => r.node)))
      .then((nodes) => {
        if (!cancelled) setAncestors(nodes);
      })
      .catch(() => {
        if (!cancelled) setAncestors([]);
      });

    return () => {
      cancelled = true;
    };
  }, [folder]);

  return (
    <nav className="crumbs" aria-label="Breadcrumb">
      <button onClick={() => onNavigate(undefined)}>Home</button>
      {ancestors.map((a) => (
        <span key={a.id} className="row" style={{ gap: "0.25rem" }}>
          <span className="sep">/</span>
          <button onClick={() => onNavigate(a.id)}>{a.name}</button>
        </span>
      ))}
      {folder && folder.name && (
        <>
          <span className="sep">/</span>
          <strong style={{ padding: "0.2rem 0.35rem" }}>{folder.name}</strong>
        </>
      )}
    </nav>
  );
}

function QuotaBar({ usage }: { usage: Usage }) {
  if (usage.quota_bytes === undefined) {
    return (
      <span className="muted small">
        {formatBytes(usage.total_bytes)} used · {usage.file_count} files
      </span>
    );
  }
  const pct = Math.min(100, (usage.total_bytes / usage.quota_bytes) * 100);
  return (
    <div className="quota stack" style={{ gap: "0.2rem" }}>
      <span className="muted small">
        {formatBytes(usage.total_bytes)} of {formatBytes(usage.quota_bytes)}
        {/* Trash is called out separately: "delete something to free space" is
            only true once the trash is emptied, and hiding that makes the
            number look like a lie. */}
        {usage.trash_bytes > 0 && ` · ${formatBytes(usage.trash_bytes)} in trash`}
      </span>
      <div className="progress">
        <div style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}
