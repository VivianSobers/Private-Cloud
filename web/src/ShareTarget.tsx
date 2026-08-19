import { useCallback, useEffect, useState } from "react";

import { api, formatBytes, type Node } from "./api";
import { clearInbox, discard, readInbox, type SharedItem } from "./share";
import { upload } from "./upload";

// What the phone's share sheet lands on.
//
// The service worker has already taken the files off the POST and parked them
// (see share.ts). This is the half that asks the one question the share sheet
// cannot: where do these go. Nothing is uploaded until that is answered — a
// share that silently wrote into some default folder would be the one mistake
// that cannot be undone from a phone.

type State = "waiting" | "sending" | "sent" | "failed";

interface Row {
  item: SharedItem;
  state: State;
  sent: number;
  error?: string;
}

export function ShareTarget({ onDone }: { onDone: () => void }) {
  const [rows, setRows] = useState<Row[] | null>(null);
  const [dest, setDest] = useState<Node | null>(null);
  const [folders, setFolders] = useState<Node[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      try {
        const items = await readInbox();
        setRows(items.map((item) => ({ item, state: "waiting" as State, sent: 0 })));
      } catch {
        setRows([]);
      }
    })();
  }, []);

  // The destination starts at your own root and is navigated downwards. Loading
  // it here rather than in the same effect keeps a failure to reach the server
  // from also hiding the files that already arrived.
  const openFolder = useCallback(async (id?: string) => {
    try {
      const at = id ?? (await api.root()).node.id;
      const res = await api.children(at);
      setDest(res.parent);
      setFolders(res.children.filter((n) => n.kind === "folder" && !n.trashed_at));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not load folders");
    }
  }, []);

  useEffect(() => {
    void openFolder();
  }, [openFolder]);

  async function sendAll() {
    if (!dest || !rows) return;
    setBusy(true);
    for (const [i, row] of rows.entries()) {
      if (row.state === "sent") continue;
      // Sequential on purpose: a share is usually a handful of photos over a
      // phone connection, and three concurrent transfers there is slower than
      // one, not faster.
      await sendOne(i, row, dest.id);
    }
    setBusy(false);
  }

  function sendOne(index: number, row: Row, destId: string): Promise<void> {
    const patch = (next: Partial<Row>) =>
      setRows((cur) => cur?.map((r, i) => (i === index ? { ...r, ...next } : r)) ?? cur);

    return new Promise((resolve) => {
      patch({ state: "sending", sent: 0, error: undefined });
      // A Blob out of the cache has no name; the name travelled in a header and
      // is put back here, because it is what the file will be called forever.
      const file = new File([row.item.blob], row.item.name, {
        type: row.item.type || "application/octet-stream",
      });
      upload(file, destId, {
        onProgress: (sent) => patch({ sent }),
        onDone: () => {
          // Dropped from the inbox as soon as it is safely stored, so a reload
          // in the middle of a share does not offer the same file twice.
          void discard(row.item.key).finally(() => {
            patch({ state: "sent", sent: row.item.size });
            resolve();
          });
        },
        onError: (message) => {
          patch({ state: "failed", error: message });
          resolve();
        },
      });
    });
  }

  if (rows === null) return <p className="muted">Reading what was shared…</p>;

  if (rows.length === 0) {
    return (
      <section className="stack">
        <h2 style={{ margin: 0 }}>Nothing to add</h2>
        <p className="muted">
          The share didn't bring any files with it — or they've already been added.
        </p>
        <div className="row">
          <button className="primary" onClick={onDone}>
            Back to your files
          </button>
        </div>
      </section>
    );
  }

  const outstanding = rows.filter((r) => r.state !== "sent").length;

  return (
    <section className="stack share-target">
      <div className="stack" style={{ gap: "0.25rem" }}>
        <h2 style={{ margin: 0 }}>
          Add {rows.length === 1 ? "this file" : `these ${rows.length} files`}
        </h2>
        <p className="muted small">Shared from another app. Choose where they go.</p>
      </div>

      {error && <div className="banner error">{error}</div>}

      <div className="stack share-dest">
        <div className="row" style={{ alignItems: "baseline" }}>
          <strong>Destination</strong>
          <span className="muted small" style={{ flex: 1 }}>
            {dest ? dest.path || "/" : "…"}
          </span>
          {dest?.parent_id && (
            <button className="link" onClick={() => void openFolder(dest.parent_id)}>
              Up
            </button>
          )}
          {dest?.parent_id && (
            <button className="link" onClick={() => void openFolder()}>
              Home
            </button>
          )}
        </div>
        {folders.length > 0 && (
          <div className="row small">
            {folders.map((f) => (
              <button key={f.id} className="tag" onClick={() => void openFolder(f.id)}>
                📁 {f.name}
              </button>
            ))}
          </div>
        )}
      </div>

      <ul className="shared-list">
        {rows.map((r) => (
          <li key={r.item.key} className="row shared-item">
            <span className="shared-icon" aria-hidden="true">
              {r.item.type.startsWith("image/") ? "🖼️" : r.item.type.startsWith("video/") ? "🎬" : "📄"}
            </span>
            <span className="stack" style={{ flex: 1, gap: 0 }}>
              <span>{r.item.name}</span>
              <span className="muted small">
                {formatBytes(r.item.size)}
                {r.state === "sending" && r.item.size > 0
                  ? ` · ${Math.round((r.sent / r.item.size) * 100)}%`
                  : ""}
                {r.state === "sent" ? " · added" : ""}
                {r.state === "failed" ? ` · ${r.error ?? "failed"}` : ""}
              </span>
            </span>
            {r.state !== "sent" && (
              <button
                className="link danger"
                disabled={busy}
                onClick={() => {
                  void discard(r.item.key);
                  setRows((cur) => cur?.filter((x) => x.item.key !== r.item.key) ?? cur);
                }}
              >
                Skip
              </button>
            )}
          </li>
        ))}
      </ul>

      <div className="row">
        <button className="primary" disabled={busy || !dest || outstanding === 0} onClick={() => void sendAll()}>
          {busy ? "Adding…" : `Add to ${dest?.name || "your files"}`}
        </button>
        <button
          disabled={busy}
          onClick={() => {
            // "Not now" throws the share away rather than leaving it to reappear
            // on some unrelated visit weeks later.
            void clearInbox().finally(onDone);
          }}
        >
          {outstanding === rows.length ? "Not now" : "Done"}
        </button>
      </div>
    </section>
  );
}
