import { useCallback, useEffect, useState } from "react";

import { ApiError, api, formatBytes, formatDate, type Node } from "./api";

export function Trash({ onClose }: { onClose: () => void }) {
  const [items, setItems] = useState<Node[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setItems((await api.trash()).items);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function guard(fn: () => Promise<unknown>) {
    setError(null);
    try {
      await fn();
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <div className="stack">
      <div className="row">
        <button onClick={onClose}>← Back to files</button>
        <span style={{ flex: 1 }} />
        <button
          className="danger"
          disabled={items.length === 0}
          onClick={() =>
            void guard(async () => {
              if (!window.confirm("Permanently delete everything in the trash? This cannot be undone.")) return;
              await api.emptyTrash();
            })
          }
        >
          Empty trash
        </button>
      </div>

      {error && <div className="banner error">{error}</div>}

      <p className="muted small">
        Deleted items still occupy disk and still count against your quota until
        they are purged — the bytes really are still there. They are removed
        automatically after the retention window.
      </p>

      {loading ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <div className="empty">The trash is empty.</div>
      ) : (
        <table className="listing">
          <thead>
            <tr>
              <th>Name</th>
              <th style={{ textAlign: "right" }}>Size</th>
              <th className="when" style={{ textAlign: "right" }}>
                Deleted
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {items.map((n) => (
              <tr key={n.id}>
                <td className="name">
                  {n.kind === "folder" ? "📁" : "📄"} {n.name}
                  <div className="muted small">{n.path}</div>
                </td>
                <td className="size">{n.kind === "file" ? formatBytes(n.size ?? 0) : "—"}</td>
                <td className="when">{n.trashed_at ? formatDate(n.trashed_at) : ""}</td>
                <td className="actions">
                  <button
                    className="link"
                    onClick={() =>
                      void guard(async () => {
                        await api.restore(n.id);
                      })
                    }
                  >
                    Restore
                  </button>
                  <button
                    className="link danger"
                    onClick={() =>
                      void guard(async () => {
                        if (!window.confirm(`Permanently delete "${n.name}"? This cannot be undone.`)) return;
                        await api.purge(n.id);
                      })
                    }
                  >
                    Delete forever
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
