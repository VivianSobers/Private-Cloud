import { useCallback, useEffect, useState } from "react";

import { ApiError, api, formatDate, type ShareInfo } from "./api";

// The owner's view of every public link they have created. The token is not
// shown — it is never stored — so this manages links by their status and lets
// them be revoked, which takes effect on the next public request.
export function Shares({ onClose }: { onClose: () => void }) {
  const [items, setItems] = useState<ShareInfo[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setItems((await api.shares()).shares);
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

  async function revoke(s: ShareInfo) {
    setError(null);
    try {
      await api.revokeShare(s.id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  function status(s: ShareInfo): string {
    if (s.revoked) return "revoked";
    if (s.expired) return "expired";
    if (s.file_trashed) return "file deleted";
    if (!s.active) return "spent";
    return "active";
  }

  return (
    <div className="stack">
      <div className="row">
        <button onClick={onClose}>← Back to files</button>
      </div>

      {error && <div className="banner error">{error}</div>}

      <p className="muted small">
        Public links you have created. The link text is shown only once, when it
        is created; here you can see how each is doing and revoke it. Revocation
        takes effect immediately.
      </p>

      {loading ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <div className="empty">You have not shared anything.</div>
      ) : (
        <table className="listing">
          <thead>
            <tr>
              <th>File</th>
              <th>Status</th>
              <th style={{ textAlign: "right" }}>Downloads</th>
              <th className="when" style={{ textAlign: "right" }}>
                Created
              </th>
              <th />
            </tr>
          </thead>
          <tbody>
            {items.map((s) => (
              <tr key={s.id}>
                <td className="name">
                  {s.file_name}
                  <div className="muted small">
                    {s.path}
                    {s.has_password && " · password"}
                    {s.max_downloads != null && ` · cap ${s.max_downloads}`}
                    {s.expires_at && ` · expires ${formatDate(s.expires_at)}`}
                  </div>
                </td>
                <td className={status(s) === "active" ? "" : "muted"}>{status(s)}</td>
                <td className="size">{s.download_count}</td>
                <td className="when">{formatDate(s.created_at)}</td>
                <td className="actions">
                  {!s.revoked && (
                    <button className="link danger" onClick={() => void revoke(s)}>
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
