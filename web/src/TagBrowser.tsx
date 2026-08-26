import { useCallback, useEffect, useState } from "react";

import { ApiError, api, formatBytes, formatDate, type Node } from "./api";
import { ownershipLabel } from "./access";

// Browse the whole library by tag: a cloud of every tag with its file count, and
// the files under whichever tag is selected. The tags themselves come from the
// worker (MIME + OCR) and from what users add by hand.
// includeShared carries the Phase 7 opt-in in from the browser. It is a
// parameter rather than a setting for the reason the whole opt-in exists: the
// default listing has to keep meaning "my files", so a tag browsed from your own
// root asks for nothing and gets exactly what it got before grants existed. An
// older server ignores the parameter and answers owner-only — the view then
// renders the same rows without ownership markers, which is the truth on that
// server, so nothing needs to detect the version.
export function TagBrowser({
  onClose,
  onOpenFolder,
  includeShared = false,
}: {
  onClose: () => void;
  onOpenFolder: (id: string) => void;
  includeShared?: boolean;
}) {
  const PAGE = 50;
  const [tags, setTags] = useState<{ tag: string; count: number }[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [nodes, setNodes] = useState<Node[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Separate from `loading`, which covers the initial tag-cloud fetch. Without
  // it, switching tags rendered "No files under this tag" against the previous
  // tag's emptied list while the new one was still in flight.
  const [loadingNodes, setLoadingNodes] = useState(false);

  useEffect(() => {
    void api
      .listTags()
      .then((r) => setTags(r.tags))
      .catch((err) => setError(err instanceof ApiError ? err.message : String(err)))
      .finally(() => setLoading(false));
  }, []);

  const openTag = useCallback(async (tag: string) => {
    setSelected(tag);
    setError(null);
    setNodes([]);
    setHasMore(false);
    setLoadingNodes(true);
    try {
      const res = await api.tagNodes(tag, { limit: PAGE, includeShared });
      setNodes(res.nodes);
      setHasMore(res.has_more);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoadingNodes(false);
    }
  }, [includeShared]);

  // The API has always reported has_more here; nothing ever asked for the next
  // page, so a tag with more than a page of files silently showed only the first.
  const loadMore = useCallback(async () => {
    if (!selected) return;
    setLoadingNodes(true);
    try {
      const res = await api.tagNodes(selected, { limit: PAGE, offset: nodes.length, includeShared });
      setNodes((ns) => [...ns, ...res.nodes]);
      setHasMore(res.has_more);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoadingNodes(false);
    }
  }, [selected, nodes.length, includeShared]);

  return (
    <div className="stack">
      <div className="row">
        <strong>Browse by tag</strong>
        <span style={{ flex: 1 }} />
        <button onClick={onClose}>Back to files</button>
      </div>

      {error && <div className="banner error">{error}</div>}

      {loading ? (
        <p className="muted">Loading…</p>
      ) : tags.length === 0 ? (
        <p className="muted small">
          No tags yet. The worker adds tags to files as it processes them, and you
          can add your own from a file’s Tags dialog.
        </p>
      ) : (
        <div className="row" style={{ flexWrap: "wrap", gap: "0.4rem" }}>
          {tags.map((t) => (
            <button
              key={t.tag}
              className={selected === t.tag ? "tag active" : "tag"}
              onClick={() => void openTag(t.tag)}
            >
              {t.tag} <span className="muted small">{t.count}</span>
            </button>
          ))}
        </div>
      )}

      {selected && (
        <div className="stack">
          <p className="muted small">Files tagged “{selected}”</p>
          {loadingNodes && nodes.length === 0 ? (
            <p className="muted">Loading…</p>
          ) : nodes.length === 0 ? (
            <div className="empty">No files under this tag.</div>
          ) : (
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
                {nodes.map((n) => (
                  <tr key={n.id}>
                    <td className="name">
                      {n.kind === "folder" ? (
                        <button onClick={() => onOpenFolder(n.id)}>📁 {n.name}</button>
                      ) : (
                        <a href={api.downloadUrl(n.id)} target="_blank" rel="noreferrer">
                          📄 {n.name}
                        </a>
                      )}
                      <div className="muted small">
                        {n.path}
                        {/* A tag listing spans folders by construction, so it can
                            mix owners in a way no single banner could describe. */}
                        {ownershipLabel(n) && ` · ${ownershipLabel(n)}`}
                      </div>
                    </td>
                    <td className="size">{n.kind === "file" ? formatBytes(n.size ?? 0) : "—"}</td>
                    <td className="when">{formatDate(n.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {hasMore && (
            <button disabled={loadingNodes} onClick={() => void loadMore()}>
              {loadingNodes ? "Loading…" : "Load more"}
            </button>
          )}
        </div>
      )}
    </div>
  );
}
