import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api, formatBytes, formatDate, type Node, type Version } from "./api";

// Version history for one file, in a native <dialog> so the browser handles the
// backdrop, Escape-to-close and focus trapping rather than a hand-rolled modal.
export function Versions({
  node,
  onClose,
  onRestored,
}: {
  node: Node;
  onClose: () => void;
  onRestored: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const [versions, setVersions] = useState<Version[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setVersions((await api.versions(node.id)).versions);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [node.id]);

  useEffect(() => {
    // showModal (not the open attribute) is what draws the backdrop and traps focus.
    ref.current?.showModal();
    void load();
  }, [load]);

  async function restore(v: Version) {
    setError(null);
    try {
      await api.restoreVersion(node.id, v.id);
      // Reload the folder so the new head shows, then dismiss.
      onRestored();
      onClose();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  return (
    <dialog ref={ref} onClose={onClose} onCancel={onClose}>
      <div className="stack">
        <div className="row">
          <strong>Version history — {node.name}</strong>
          <span style={{ flex: 1 }} />
          <button onClick={onClose}>Close</button>
        </div>

        {error && <div className="banner error">{error}</div>}

        <p className="muted small">
          Restoring a version brings its content back as a new version — the ones
          in between are kept, so a restore is itself undoable.
        </p>

        {loading ? (
          <p className="muted">Loading…</p>
        ) : (
          <table className="listing">
            <thead>
              <tr>
                <th>When</th>
                <th style={{ textAlign: "right" }}>Size</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => (
                <tr key={v.id}>
                  <td className="name">
                    {formatDate(v.created_at)}
                    {v.is_head && <span className="muted small"> · current</span>}
                  </td>
                  <td className="size">{formatBytes(v.size)}</td>
                  <td className="actions">
                    <a
                      className="link"
                      href={api.versionDownloadUrl(node.id, v.id, true)}
                      target="_blank"
                      rel="noreferrer"
                    >
                      Download
                    </a>
                    {!v.is_head && (
                      <button className="link" onClick={() => void restore(v)}>
                        Restore
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </dialog>
  );
}
