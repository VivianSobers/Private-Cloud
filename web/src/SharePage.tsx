import { useCallback, useEffect, useState } from "react";

import { ApiError, api, formatBytes, type ShareView } from "./api";

// The public landing page for /s/{token}. It runs with no session — the token is
// the credential — and shows only what the server reveals: a file to download, or
// a folder to browse within, and never anything about the owner or their tree.
export function SharePage({ token }: { token: string }) {
  const [view, setView] = useState<ShareView | null>(null);
  const [path, setPath] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(
    async (p: string) => {
      setLoading(true);
      try {
        setView(await api.shareView(token, p));
        setError(null);
      } catch (err) {
        setError(err instanceof ApiError ? err.message : String(err));
        setView(null);
      } finally {
        setLoading(false);
      }
    },
    [token],
  );

  useEffect(() => {
    void load(path);
  }, [load, path]);

  async function unlock(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await api.unlockShare(token, password);
      await load(path);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    }
  }

  function open(name: string) {
    setPath(path ? `${path}/${name}` : name);
  }

  function up() {
    const i = path.lastIndexOf("/");
    setPath(i >= 0 ? path.slice(0, i) : "");
  }

  return (
    <div className="app">
      <header className="top">
        <h1>private cloud</h1>
        <span className="muted small">shared link</span>
      </header>

      <div className="card stack" style={{ maxWidth: "40rem", margin: "2rem auto" }}>
        {loading ? (
          <p className="muted">Loading…</p>
        ) : error ? (
          <div className="stack">
            <div className="banner error">{error}</div>
            <p className="muted small">This link may have been revoked, expired, or never existed.</p>
          </div>
        ) : view && view.has_password && !view.unlocked ? (
          <form className="stack" onSubmit={unlock}>
            <strong>This link is password protected.</strong>
            <input
              type="password"
              autoFocus
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
            />
            <button className="primary" type="submit">
              Unlock
            </button>
          </form>
        ) : view && view.kind === "file" ? (
          <div className="stack">
            <strong>{view.name}</strong>
            <p className="muted small">{formatBytes(view.size ?? 0)}</p>
            <div className="row">
              <a className="primary button" href={api.shareContentUrl(token, path)} target="_blank" rel="noreferrer">
                Open
              </a>
              <a href={api.shareContentUrl(token, path, true)} download>
                Download
              </a>
            </div>
          </div>
        ) : view ? (
          <div className="stack">
            <div className="row">
              <strong>{view.name || "Shared folder"}</strong>
              <span style={{ flex: 1 }} />
              {path && (
                <button className="link" onClick={up}>
                  ↑ Up
                </button>
              )}
            </div>
            {path && <p className="muted small">/{path}</p>}
            {view.entries && view.entries.length > 0 ? (
              <table className="listing">
                <tbody>
                  {view.entries.map((e) => (
                    <tr key={e.name}>
                      <td className="name">
                        {e.kind === "folder" ? (
                          <button onClick={() => open(e.name)}>📁 {e.name}</button>
                        ) : (
                          <a
                            href={api.shareContentUrl(token, path ? `${path}/${e.name}` : e.name)}
                            target="_blank"
                            rel="noreferrer"
                          >
                            📄 {e.name}
                          </a>
                        )}
                      </td>
                      <td className="size">{e.kind === "file" ? formatBytes(e.size ?? 0) : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="empty">This folder is empty.</div>
            )}
          </div>
        ) : null}
      </div>
    </div>
  );
}
