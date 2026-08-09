import { useEffect, useState } from "react";

import { api, ApiError, type Node } from "./api";

// "Shared with me": the roots other users have granted me access to. Reads the
// Phase 7 `GET /shared` surface. Each item carries an `access` object naming the
// owner and my role; a file can be downloaded, a folder shows where it lives.

export function SharedWithMe() {
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
                <strong>{n.name}</strong>
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
        Opening a shared folder inline is coming with the shared browser; for now a
        shared folder shows where it lives in its owner's tree.
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
