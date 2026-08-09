import { useEffect, useRef, useState } from "react";

import { ApiError, api, type CreatedShare, type Node } from "./api";
import { PeopleShare } from "./PeopleShare";

// Create a public link for one file or folder. The token comes back exactly once
// — it is stored only hashed — so the created link is shown here to copy now, and
// can never be retrieved again.
export function ShareDialog({ node, onClose }: { node: Node; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [password, setPassword] = useState("");
  const [expiresInHours, setExpiresInHours] = useState("");
  const [maxDownloads, setMaxDownloads] = useState("");
  const [created, setCreated] = useState<CreatedShare | null>(null);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    ref.current?.showModal();
  }, []);

  // The public URL lives on the share host in production; the origin is the best
  // default the client can offer, and it is correct in dev where the planes share
  // one origin.
  const url = created ? `${window.location.origin}${created.path}` : "";

  async function create() {
    setBusy(true);
    setError(null);
    try {
      const res = await api.createShare(node.id, {
        password: password || undefined,
        expiresInHours: expiresInHours ? Number(expiresInHours) : undefined,
        maxDownloads: maxDownloads ? Number(maxDownloads) : undefined,
      });
      setCreated(res.share);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      // Clipboard can be blocked; the field is selectable as a fallback.
    }
  }

  return (
    <dialog ref={ref} onClose={onClose} onCancel={onClose}>
      <div className="stack">
        <div className="row">
          <strong>Share “{node.name}”</strong>
          <span style={{ flex: 1 }} />
          <button onClick={onClose}>Close</button>
        </div>

        {error && <div className="banner error">{error}</div>}

        <PeopleShare node={node} />

        <hr className="divider" />
        <strong className="small">Public link</strong>

        {created ? (
          <>
            <p className="muted small">
              Copy this link now — it cannot be shown again. Anyone with it
              {created.has_password ? " and the password" : ""} can read this{" "}
              {node.kind}.
            </p>
            <div className="row">
              <input readOnly value={url} style={{ flex: 1 }} onFocus={(e) => e.target.select()} />
              <button className="primary" onClick={() => void copy()}>
                {copied ? "Copied" : "Copy"}
              </button>
            </div>
          </>
        ) : (
          <>
            <p className="muted small">
              A read-only public link. Leave the options blank for a plain link,
              or add a password, an expiry, or a download limit.
            </p>
            <label className="stack small">
              Password (optional)
              <input
                type="password"
                autoComplete="new-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="no password"
              />
            </label>
            <label className="stack small">
              Expires after (hours, optional)
              <input
                type="number"
                min="0"
                value={expiresInHours}
                onChange={(e) => setExpiresInHours(e.target.value)}
                placeholder="never"
              />
            </label>
            <label className="stack small">
              Maximum downloads (optional)
              <input
                type="number"
                min="0"
                value={maxDownloads}
                onChange={(e) => setMaxDownloads(e.target.value)}
                placeholder="unlimited"
              />
            </label>
            <div className="row">
              <span style={{ flex: 1 }} />
              <button className="primary" disabled={busy} onClick={() => void create()}>
                {busy ? "Creating…" : "Create link"}
              </button>
            </div>
          </>
        )}
      </div>
    </dialog>
  );
}
