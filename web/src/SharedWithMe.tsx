import { useEffect, useState } from "react";

import { api, ApiError, type Node } from "./api";

// "Shared with me": the roots other users have granted me access to. Reads the
// Phase 7 `GET /shared` surface. Each item carries an `access` object naming the
// owner and my role; a file can be downloaded, and a folder opens in the file
// browser through the `?include_shared=true` opt-in the server has supported
// since Phase 7 slice 2.

export function SharedWithMe({ onOpenFolder }: { onOpenFolder?: (id: string) => void } = {}) {
  const [items, setItems] = useState<Node[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api
      .shared()
      .then((r) => live && setItems(r.items))
      .catch((e) => live && setError(describe(e)));
    return () => {
      live = false;
    };
  }, []);

  if (error)
    return (
      <section className="stack">
        <p className="muted">Sharing between users isn't available on this server yet.</p>
        <p className="muted small">{error}</p>
      </section>
    );
  if (!items) return <p className="muted">Loading…</p>;
  if (items.length === 0)
    return <p className="muted">Nothing has been shared with you yet.</p>;

  return (
    <section className="stack">
      <ul className="shared-list">
        {items.map((n) => (
          <li key={n.id} className="row shared-item">
            <span className="shared-icon" aria-hidden="true">
              {n.kind === "folder" ? "📁" : "📄"}
            </span>
            <span className="stack" style={{ flex: 1, gap: 0 }}>
              <span>
                {n.kind === "folder" && onOpenFolder ? (
                  <button className="link" onClick={() => onOpenFolder(n.id)}>
                    <strong>{n.name}</strong>
                  </button>
                ) : (
                  <strong>{n.name}</strong>
                )}
                {n.access && <span className="role-badge">{n.access.role}</span>}
              </span>
              <span className="muted small">
                {n.access ? `shared by ${n.access.owner}` : ""} · {n.path}
              </span>
            </span>
            {n.kind === "file" && (
              <a className="link" href={api.downloadUrl(n.id, true)}>
                Download
              </a>
            )}
          </li>
        ))}
      </ul>
      <p className="muted small">
        Opening a shared folder browses it in place, in its owner's tree. Your own
        listings are unchanged: shared content appears only where you asked for it.
      </p>
    </section>
  );
}

function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 404) return "The /shared endpoint is not available on this server yet.";
    return `${e.code}: ${e.message}`;
  }
  return e instanceof Error ? e.message : "Unknown error";
}
